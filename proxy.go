package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/google/uuid"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	"github.com/sourcegraph/go-langserver/pkg/lsp"
	"github.com/sourcegraph/jsonrpc2"
)

var (
	proxyAddr          = flag.String("proxyAddress", "127.0.0.1:8080", "proxy server listen address (tcp)")
	pprofAddr          = flag.String("pprofAddr", "", "server listen address for pprof")
	cacheDir           *string
	unresolvedCacheDir = flag.String("cacheDirectory", filepath.Join(os.TempDir(), "proxy-cache"), "cache directory location")
	didOpenLanguage    = flag.String("didOpenLanguage", "", "(HACK) If non-empty, send 'textDocument/didOpen' notifications with the specified language field (e.x. 'python') to the language server for every file.")
	jsonrpc2IDRewrite  = flag.String("jsonrpc2IDRewrite", "none", "(HACK) Rewrite jsonrpc2 ID. none (default) is no rewriting. string will use a string ID. number will use number ID. Useful for language servers with non-spec complaint JSONRPC2 implementations.")
	glob               = flag.String("glob", "", "A colon (:) separated list of file globs to sync locally. By default we place all files into the workspace, but some language servers may only look at a subset of files. Specifying this allows us to avoid syncing all files. Note: This is done by basename only.")
	beforeInitHook     = flag.String("beforeInitializeHook", "", "A program to run after cloning the repository, but before the 'initialize' call is forwarded to the language server. (For example, you can use this to run a script to install dependencies for the project). The program's cwd will be the workspace's cache directory, and it will also be passed the cache directory as an argument.")
	trace              = flag.Bool("trace", true, "trace logs to stderr")
)

type cloneProxy struct {
	client *jsonrpc2.Conn // connection to the browser
	server *jsonrpc2.Conn // connection to the language server

	sessionID     uuid.UUID      // unique ID for this session
	lastRequestID *atomicCounter // counter that is incremented for each new request that is sent across the wire for this session

	ready chan struct{} // barrier to block handling requests until the proxy is fully initialized
	ctx   context.Context

	// HACK
	didOpenMu sync.Mutex
	didOpen   map[string]bool
}

func (p *cloneProxy) start() {
	close(p.ready)
}

type jsonrpc2HandlerFunc func(context.Context, *jsonrpc2.Conn, *jsonrpc2.Request)

func (h jsonrpc2HandlerFunc) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	h(ctx, conn, req)
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [OPTIONS] LSP_COMMAND_ARGS...\n\nOptions:\n", os.Args[0])
		flag.PrintDefaults()
	}

	flag.Parse()
	log.SetFlags(log.Flags() | log.Lshortfile)

	lspBin := flag.Args()
	if len(lspBin) == 0 {
		log.Fatal("You must specify an LSP command (positional arguments).")
	}

	switch *jsonrpc2IDRewrite {
	case "none", "string", "number":
	default:
		log.Fatalf("Invalid jsonrpc2IDRewrite value %q", *jsonrpc2IDRewrite)
	}

	// Ensure the path exists, otherwise symlinks to it cannot be resolved.
	if err := os.MkdirAll(*unresolvedCacheDir, os.ModePerm); err != nil {
		log.Fatalf("Error when checking -cacheDirectory=%q to check if it exists: %s", *unresolvedCacheDir, err)
	}

	// Resolve symlinks to avoid path mismatches in situations where the
	// language server resolves symlinks. One example is the Swift language
	// server: https://github.com/sourcegraph/sourcegraph/issues/11867
	resolvedCacheDir, err := filepath.EvalSymlinks(*unresolvedCacheDir)
	if err != nil {
		log.Fatalf("Could not resolve symlinks in -cacheDirectory=%q because: %s", *unresolvedCacheDir, err)
	}
	cacheDir = &resolvedCacheDir

	lis, err := net.Listen("tcp", *proxyAddr)
	if err != nil {
		err = errors.Wrap(err, "setting up proxy listener failed")
		log.Fatal(err)
	}

	log.Printf("CloneProxy: accepting connections at %s", lis.Addr())

	if *pprofAddr != "" {
		go debugServer(*pprofAddr)
	}

	ctx, cancel := context.WithCancel(context.Background())

	shutdown := func() {
		cancel()
		lis.Close()

		// Remove the entire cache when the program is exiting
		os.RemoveAll(*cacheDir)
	}

	defer shutdown()
	go trapSignalsForShutdown(shutdown)

	var wg sync.WaitGroup
	for {
		clientNetConn, err := lis.Accept()
		if err != nil {
			if ctx.Err() != nil { // shutdown
				break
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				log.Println("error when accepting client connection: ", err.Error())
				continue
			}
			log.Fatal(err)
		}

		wg.Add(1)
		go func(clientNetConn net.Conn) {
			defer wg.Done()

			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			var lsConn, err = stdIoLSConn(ctx, lspBin[0], lspBin[1:]...)
			if err != nil {
				log.Println("connecting to language server over stdio failed", err.Error())
				return
			}

			proxy := &cloneProxy{
				ready:         make(chan struct{}),
				ctx:           ctx,
				sessionID:     uuid.New(),
				lastRequestID: newAtomicCounter(),
				didOpen:       map[string]bool{},
			}
			traceID := proxy.sessionID.String()

			var serverConnOpts []jsonrpc2.ConnOpt
			if *trace {
				serverConnOpts = append(serverConnOpts, jsonrpc2.LogMessages(log.New(os.Stderr, fmt.Sprintf("TRACE %s ", traceID), log.Ltime)))
			}
			if *pprofAddr != "" {
				serverConnOpts = append(serverConnOpts, traceRequests(traceID), traceEventLog("server", traceID))
			}
			proxy.client = jsonrpc2.NewConn(ctx, jsonrpc2.NewBufferedStream(clientNetConn, jsonrpc2.VSCodeObjectCodec{}), jsonrpc2.AsyncHandler(jsonrpc2HandlerFunc(proxy.handleClientRequest)))
			proxy.server = jsonrpc2.NewConn(ctx, jsonrpc2.NewBufferedStream(lsConn, jsonrpc2.VSCodeObjectCodec{}), jsonrpc2.AsyncHandler(jsonrpc2HandlerFunc(proxy.handleServerRequest)), serverConnOpts...)

			proxy.start()

			// When one side of the connection disconnects, close the other side.
			select {
			case <-proxy.client.DisconnectNotify():
				proxy.server.Close()
			case <-proxy.server.DisconnectNotify():
				proxy.client.Close()
			}

			// Remove the cache contents for this workspace after the connection closes
			proxy.cleanWorkspaceCache()
		}(clientNetConn)
	}

	wg.Wait()
}

func (p *cloneProxy) handleServerRequest(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	<-p.ready

	rTripper := roundTripper{
		req:             req,
		globalRequestID: p.lastRequestID,

		src:  p.server,
		dest: p.client,

		updateURIFromSrc:  func(uri lsp.DocumentURI) lsp.DocumentURI { return serverToClientURI(uri, p.workspaceCacheDir()) },
		updateURIFromDest: func(uri lsp.DocumentURI) lsp.DocumentURI { return clientToServerURI(uri, p.workspaceCacheDir()) },
	}

	if err := rTripper.roundTrip(ctx); err != nil {
		log.Println("CloneProxy.handleServerRequest(): roundTrip failed", err)
	}
}

func (p *cloneProxy) handleClientRequest(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	<-p.ready

	if req.Method == "initialize" {
		globs := strings.FieldsFunc(*glob, func(r rune) bool { return r == ':' })
		if err := p.cloneWorkspaceToCache(globs); err != nil {
			log.Println("CloneProxy.handleClientRequest(): cloning workspace failed during initialize", err)
			return
		}
		if *beforeInitHook != "" {
			if err := p.runHook(ctx, *beforeInitHook); err != nil {
				log.Println("CloneProxy.handleClientRequest(): running beforeInitializeHook failed", err)
			}
		}
	} else if req.Method == "workspace/didChangeWorkspaceFolders" {
		globs := strings.FieldsFunc(*glob, func(r rune) bool { return r == ':' })

		var params map[string]interface{}
		json.Unmarshal(*req.Params, &params)
		event := params["event"].(map[string]interface{})
		addFolders := event["added"].([]interface {})
		removedFolders := event["removed"].([]interface {})

		if len(removedFolders) > 0 {
			for _, s := range removedFolders {
				item := s.(map[string]interface{})
				workspaceName := item["name"].(string)
				err := p.removeWorkspaceCache(workspaceName)
				if (err != nil) {
					log.Println("CloneProxy.handleClientRequest(): remove workspace failed during initialize", err)
				}
			}
		}

		if len(addFolders) > 0 {
			if err := p.cloneWorkspaceToCache(globs); err != nil {
				log.Println("CloneProxy.handleClientRequest(): cloning workspace failed during initialize", err)
				return
			}
		}
	}

	rTripper := roundTripper{
		req:             req,
		globalRequestID: p.lastRequestID,

		src:  p.client,
		dest: p.server,

		updateURIFromSrc: func(uri lsp.DocumentURI) lsp.DocumentURI {
			uri = clientToServerURI(uri, p.workspaceCacheDir())

			// HACK
			//
			// Some language servers don't follow LSP correctly, and refuse to handle requests that
			// operate on files that the language server hasn't received a a 'textDocument/didOpen'
			// request for yet.
			//
			// See this issue for more context: https://github.com/Microsoft/language-server-protocol/issues/177
			// There is also a corresponding PR to officially put this clarification in the text:
			// https://github.com/Microsoft/language-server-protocol/pull/431
			//
			// This hack is necessary to get those offending language servers to work at all.
			//
			// This is not indended to be a robust implementation, so there is no attempt to send
			// matching 'textDocument/didClose' requests / etc.
			if *didOpenLanguage != "" {
				if parsedURI, err := url.Parse(string(uri)); err == nil && probablyFileURI(parsedURI) {
					p.didOpenMu.Lock()
					sent := p.didOpen[parsedURI.Path]
					if !sent {
						p.didOpen[parsedURI.Path] = true
					}
					p.didOpenMu.Unlock()

					if !sent {
						b, err := ioutil.ReadFile(parsedURI.Path)
						if err == nil {
							err = p.server.Notify(ctx, "textDocument/didOpen", &lsp.DidOpenTextDocumentParams{
								TextDocument: lsp.TextDocumentItem{
									URI:        uri,
									LanguageID: *didOpenLanguage,
									Version:    1,
									Text:       string(b),
								},
							})
							if err != nil {
								log.Println("error sending didOpen", err)
							}
						}
					}
				}
			}

			return uri
		},
		updateURIFromDest: func(uri lsp.DocumentURI) lsp.DocumentURI { return serverToClientURI(uri, p.workspaceCacheDir()) },
	}

	if err := rTripper.roundTrip(ctx); err != nil {
		log.Println("CloneProxy.handleClientRequest(): roundTrip failed", err)
	}
}

type roundTripper struct {
	req             *jsonrpc2.Request
	globalRequestID *atomicCounter

	src  *jsonrpc2.Conn
	dest *jsonrpc2.Conn

	updateURIFromSrc  func(lsp.DocumentURI) lsp.DocumentURI
	updateURIFromDest func(lsp.DocumentURI) lsp.DocumentURI
}

// roundTrip passes requests from one side of the connection to the other.
func (r *roundTripper) roundTrip(ctx context.Context) error {
	var params interface{}
	if r.req.Params != nil {
		if err := json.Unmarshal(*r.req.Params, &params); err != nil {
			return errors.Wrap(err, "unmarshling request parameters failed")
		}
	}

	WalkURIFields(params, r.updateURIFromSrc)

	if r.req.Notif {
		err := r.dest.Notify(ctx, r.req.Method, params)
		if err != nil {
			err = errors.Wrap(err, "sending notification to dest failed")
		}
		// Don't send responses back to src for Notification requests
		return err
	}

	var id jsonrpc2.ID
	switch *jsonrpc2IDRewrite {
	case "none":
		id = r.req.ID
	case "string":
		// Some language servers don't properly support ID's that are ints
		// (e.x. Clojure), so we provide a string instead. Note that doing this
		// breaks the `$/cancelRequest` and `$/partialResult` request.
		id = jsonrpc2.ID{
			Str:      strconv.FormatUint(r.globalRequestID.getAndInc(), 10),
			IsString: true,
		}
	case "number":
		// Some language servers don't properly support ID's that are strings
		// (e.x. Rust), so we provide a number instead. Note that doing this
		// breaks the `$/cancelRequest` and `$/partialResult` request.
		id = jsonrpc2.ID{
			Num: r.globalRequestID.getAndInc(),
		}
	default:
		panic("unexpected jsonrpc2IDRewrite " + *jsonrpc2IDRewrite)
	}

	var rawResult *json.RawMessage
	err := r.dest.Call(ctx, r.req.Method, params, &rawResult, jsonrpc2.PickID(id))

	if err != nil {
		var respErr *jsonrpc2.Error
		if e, ok := err.(*jsonrpc2.Error); ok {
			respErr = e
		} else {
			respErr = &jsonrpc2.Error{Message: err.Error()}
		}

		var multiErr error = respErr

		if err = r.src.ReplyWithError(ctx, r.req.ID, respErr); err != nil {
			multiErr = multierror.Append(multiErr, errors.Wrap(err, "when sending error reply back to src"))
		}

		return errors.Wrapf(multiErr, "calling method %s on dest failed", r.req.Method)
	}

	var result interface{}
	if rawResult != nil {
		if err := json.Unmarshal(*rawResult, &result); err != nil {
			return errors.Wrap(err, "unmarshling result failed")
		}
	}

	WalkURIFields(result, r.updateURIFromDest)

	if err = r.src.Reply(ctx, r.req.ID, &result); err != nil {
		return errors.Wrap(err, "sending reply to back to src failed")
	}

	return nil
}

func trapSignalsForShutdown(shutdown func()) {
	// Listen for shutdown signals. When we receive one attempt to clean up,
	// but do an insta-shutdown if we receive more than one signal.
	c := make(chan os.Signal, 2)
	signal.Notify(c, syscall.SIGINT, syscall.SIGHUP)
	<-c
	go func() {
		<-c
		os.Exit(0)
	}()

	shutdown()
}
