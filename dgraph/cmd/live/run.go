/*
 * Copyright 2017-2018 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package live

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net/http"
	_ "net/http/pprof" // http profiler
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/dgraph-io/badger"
	bopt "github.com/dgraph-io/badger/options"
	"github.com/dgraph-io/dgo"
	"github.com/dgraph-io/dgo/protos/api"
	"github.com/dgraph-io/dgraph/rdf"
	"github.com/dgraph-io/dgraph/x"
	"github.com/dgraph-io/dgraph/xidmap"
	"github.com/spf13/cobra"
)

type options struct {
	files               string
	schemaFile          string
	dgraph              string
	zero                string
	concurrent          int
	numRdf              int
	clientDir           string
	ignoreIndexConflict bool
	authToken           string
	useCompression      bool
}

var opt options
var tlsConf x.TLSHelperConfig

var Live x.SubCommand

func init() {
	Live.Cmd = &cobra.Command{
		Use:   "live",
		Short: "Run Dgraph live loader",
		Run: func(cmd *cobra.Command, args []string) {
			defer x.StartProfile(Live.Conf).Stop()
			if err := run(); err != nil {
				os.Exit(1)
			}
		},
	}
	Live.EnvPrefix = "DGRAPH_LIVE"

	flag := Live.Cmd.Flags()
	flag.StringP("rdfs", "r", "", "Location of rdf files to load")
	flag.StringP("schema", "s", "", "Location of schema file")
	flag.StringP("dgraph", "d", "127.0.0.1:9080", "Dgraph alpha gRPC server address")
	flag.StringP("zero", "z", "127.0.0.1:5080", "Dgraph zero gRPC server address")
	flag.IntP("conc", "c", 10,
		"Number of concurrent requests to make to Dgraph")
	flag.IntP("batch", "b", 1000,
		"Number of RDF N-Quads to send as part of a mutation.")
	flag.StringP("xidmap", "x", "", "Directory to store xid to uid mapping")
	flag.BoolP("ignore_index_conflict", "i", true,
		"Ignores conflicts on index keys during transaction")
	flag.StringP("auth_token", "a", "",
		"The auth token passed to the server for Alter operation of the schema file")
	flag.BoolP("use_compression", "C", false,
		"Enable compression on connection to alpha server")

	// TLS configuration
	x.RegisterTLSFlags(flag)
	flag.String("tls_server_name", "", "Used to verify the server hostname.")
}

// Reads a single line from a buffered reader. The line is read into the
// passed in buffer to minimize allocations. This is the preferred
// method for loading long lines which could be longer than the buffer
// size of bufio.Scanner.
func readLine(r *bufio.Reader, buf *bytes.Buffer) error {
	isPrefix := true
	var err error
	for isPrefix && err == nil {
		var line []byte
		// The returned line is an pb.buffer in bufio and is only
		// valid until the next call to ReadLine. It needs to be copied
		// over to our own buffer.
		line, isPrefix, err = r.ReadLine()
		if err == nil {
			buf.Write(line)
		}
	}
	return err
}

// processSchemaFile process schema for a given gz file.
func processSchemaFile(ctx context.Context, file string, dgraphClient *dgo.Dgraph) error {
	fmt.Printf("\nProcessing %s\n", file)
	if len(opt.authToken) > 0 {
		md := metadata.New(nil)
		md.Append("auth-token", opt.authToken)
		ctx = metadata.NewOutgoingContext(ctx, md)
	}

	f, err := os.Open(file)
	x.Check(err)
	defer f.Close()

	var reader io.Reader
	if strings.HasSuffix(strings.ToLower(file), ".gz") {
		reader, err = gzip.NewReader(f)
		x.Check(err)
	} else {
		reader = f
	}

	b, err := ioutil.ReadAll(reader)
	if err != nil {
		x.Checkf(err, "Error while reading file")
	}

	op := &api.Operation{}
	op.Schema = string(b)
	return dgraphClient.Alter(ctx, op)
}

func (l *loader) uid(val string) string {
	// Attempt to parse as a UID (in the same format that dgraph outputs - a
	// hex number prefixed by "0x"). If parsing succeeds, then this is assumed
	// to be an existing node in the graph. There is limited protection against
	// a user selecting an unassigned UID in this way - it may be assigned
	// later to another node. It is up to the user to avoid this.
	if strings.HasPrefix(val, "0x") {
		if _, err := strconv.ParseUint(val[2:], 16, 64); err == nil {
			return val
		}
	}

	uid, _ := l.alloc.AssignUid(val)
	return fmt.Sprintf("%#x", uint64(uid))
}

func fileReader(file string) (io.Reader, *os.File) {
	f, err := os.Open(file)
	x.Check(err)

	var r io.Reader
	if filepath.Ext(file) == ".gz" {
		r, err = gzip.NewReader(f)
		x.Check(err)
	} else {
		r = bufio.NewReader(f)
	}
	return r, f
}

// processFile sends mutations for a given gz file.
func (l *loader) processFile(ctx context.Context, file string) error {
	fmt.Printf("\nProcessing %s\n", file)
	gr, f := fileReader(file)
	var buf bytes.Buffer
	bufReader := bufio.NewReader(gr)
	defer f.Close()

	var line uint64
	mu := api.Mutation{}
	var batchSize int
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		err := readLine(bufReader, &buf)
		if err != nil {
			if err != io.EOF {
				return err
			}
			break
		}
		line++

		nq, err := rdf.Parse(buf.String())
		if err == rdf.ErrEmpty { // special case: comment/empty line
			buf.Reset()
			continue
		} else if err != nil {
			return fmt.Errorf("Error while parsing RDF: %v, on line:%v %v", err, line, buf.String())
		}
		batchSize++
		buf.Reset()

		nq.Subject = l.uid(nq.Subject)
		if len(nq.ObjectId) > 0 {
			nq.ObjectId = l.uid(nq.ObjectId)
		}
		mu.Set = append(mu.Set, &nq)

		if batchSize >= opt.numRdf {
			l.reqs <- mu
			batchSize = 0
			mu = api.Mutation{}
		}
	}
	if batchSize > 0 {
		l.reqs <- mu
		mu = api.Mutation{}
	}
	return nil
}

func fileList(files string) []string {
	if len(files) == 0 {
		return []string{}
	}
	return strings.Split(files, ",")
}

func setup(opts batchMutationOptions, dc *dgo.Dgraph) *loader {
	x.Check(os.MkdirAll(opt.clientDir, 0700))
	o := badger.DefaultOptions
	o.SyncWrites = true // So that checkpoints are persisted immediately.
	o.TableLoadingMode = bopt.MemoryMap
	o.Dir = opt.clientDir
	o.ValueDir = opt.clientDir

	kv, err := badger.Open(o)
	x.Checkf(err, "Error while creating badger KV posting store")

	// compression with zero server actually makes things worse
	connzero, err := x.SetupConnection(opt.zero, &tlsConf, false)
	x.Checkf(err, "Unable to connect to zero, Is it running at %s?", opt.zero)

	alloc := xidmap.New(
		kv,
		connzero,
		xidmap.Options{
			NumShards: 100,
			LRUSize:   1e5,
		},
	)

	l := &loader{
		opts:     opts,
		dc:       dc,
		start:    time.Now(),
		reqs:     make(chan api.Mutation, opts.Pending*2),
		alloc:    alloc,
		kv:       kv,
		zeroconn: connzero,
	}

	l.requestsWg.Add(opts.Pending)
	for i := 0; i < opts.Pending; i++ {
		go l.makeRequests()
	}

	rand.Seed(time.Now().Unix())
	return l
}

func run() error {
	x.PrintVersion()
	opt = options{
		files:               Live.Conf.GetString("rdfs"),
		schemaFile:          Live.Conf.GetString("schema"),
		dgraph:              Live.Conf.GetString("dgraph"),
		zero:                Live.Conf.GetString("zero"),
		concurrent:          Live.Conf.GetInt("conc"),
		numRdf:              Live.Conf.GetInt("batch"),
		clientDir:           Live.Conf.GetString("xidmap"),
		ignoreIndexConflict: Live.Conf.GetBool("ignore_index_conflict"),
		authToken:           Live.Conf.GetString("auth_token"),
		useCompression:      Live.Conf.GetBool("use_compression"),
	}
	x.LoadTLSConfig(&tlsConf, Live.Conf, x.TlsClientCert, x.TlsClientKey)
	tlsConf.ServerName = Live.Conf.GetString("tls_server_name")

	go http.ListenAndServe("localhost:6060", nil)
	ctx := context.Background()
	bmOpts := batchMutationOptions{
		Size:          opt.numRdf,
		Pending:       opt.concurrent,
		PrintCounters: true,
		Ctx:           ctx,
		MaxRetries:    math.MaxUint32,
	}

	ds := strings.Split(opt.dgraph, ",")
	var clients []api.DgraphClient
	for _, d := range ds {
		conn, err := x.SetupConnection(d, &tlsConf, opt.useCompression)
		x.Checkf(err, "While trying to setup connection to Dgraph alpha.")
		defer conn.Close()

		dc := api.NewDgraphClient(conn)
		clients = append(clients, dc)
	}
	dgraphClient := dgo.NewDgraphClient(clients...)

	if len(opt.clientDir) == 0 {
		var err error
		opt.clientDir, err = ioutil.TempDir("", "x")
		x.Checkf(err, "Error while trying to create temporary client directory.")
		fmt.Printf("Creating temp client directory at %s\n", opt.clientDir)
		defer os.RemoveAll(opt.clientDir)
	}
	l := setup(bmOpts, dgraphClient)
	defer l.zeroconn.Close()
	defer l.kv.Close()
	defer l.alloc.EvictAll()

	if len(opt.schemaFile) > 0 {
		if err := processSchemaFile(ctx, opt.schemaFile, dgraphClient); err != nil {
			if err == context.Canceled {
				fmt.Printf("Interrupted while processing schema file %q\n", opt.schemaFile)
				return nil
			}
			fmt.Printf("Error while processing schema file %q: %s\n", opt.schemaFile, err)
			return err
		}
		fmt.Printf("Processed schema file %q\n", opt.schemaFile)
	}

	filesList := fileList(opt.files)
	totalFiles := len(filesList)
	if totalFiles == 0 {
		return nil
	}

	//	x.Check(dgraphClient.NewSyncMarks(filesList))
	errCh := make(chan error, totalFiles)
	for _, file := range filesList {
		file = strings.Trim(file, " \t")
		go func(file string) {
			errCh <- l.processFile(ctx, file)
		}(file)
	}

	// PrintCounters should be called after schema has been updated.
	if bmOpts.PrintCounters {
		go l.printCounters()
	}

	for i := 0; i < totalFiles; i++ {
		if err := <-errCh; err != nil {
			fmt.Printf("Error while processing file %q: %s\n", filesList[i], err)
			return err
		}
	}

	close(l.reqs)
	// First we wait for requestsWg, when it is done we know all retry requests have been added
	// to retryRequestsWg. We can't have the same waitgroup as by the time we call Wait, we can't
	// be sure that all retry requests have been added to the waitgroup.
	l.requestsWg.Wait()
	l.retryRequestsWg.Wait()
	c := l.Counter()
	var rate uint64
	if c.Elapsed.Seconds() < 1 {
		rate = c.Rdfs
	} else {
		rate = c.Rdfs / uint64(c.Elapsed.Seconds())
	}
	// Lets print an empty line, otherwise Interrupted or Number of Mutations overwrites the
	// previous printed line.
	fmt.Printf("%100s\r", "")
	fmt.Printf("Number of TXs run         : %d\n", c.TxnsDone)
	fmt.Printf("Number of RDFs processed  : %d\n", c.Rdfs)
	fmt.Printf("Time spent                : %v\n", c.Elapsed)
	fmt.Printf("RDFs processed per second : %d\n", rate)

	return nil
}