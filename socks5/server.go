package socks5

import (
	"bepass/bufferpool"
	"bufio"
	"context"
	"errors"
	"fmt"
	"golang.org/x/net/proxy"
	"io"
	"log"
	"net"
	"net/http"
	"os"

	"bepass/logger"
	"bepass/socks5/statute"
	"github.com/elazarl/goproxy"
)

// GPool is used to implement custom goroutine pool default use goroutine
type GPool interface {
	Submit(f func()) error
}

// Server is responsible for accepting internet and handling
// the details of the SOCKS5 protocol
type Server struct {
	// authMethods can be provided to implement authentication
	// By default, "no-auth" mode is enabled.
	// For password-based auth use UserPassAuthenticator.
	authMethods []Authenticator
	// If provided, username/password authentication is enabled,
	// by appending a UserPassAuthenticator to AuthMethods. If not provided,
	// and authMethods is nil, then "no-auth" mode is enabled.
	credentials CredentialStore
	// resolver can be provided to do custom name resolution.
	// Defaults to DNSResolver if not provided.
	resolver NameResolver
	// rules is provided to enable custom logic around permitting
	// various commands. If not provided, NewPermitAll is used.
	rules RuleSet
	// rewriter can be used to transparently rewrite addresses.
	// This is invoked before the RuleSet is invoked.
	// Defaults to NoRewrite.
	rewriter AddressRewriter
	// bindIP is used for bind or udp associate
	bindIP net.IP
	// logger can be used to provide a custom log target.
	// Defaults to io.Discard.
	logger logger.Logger
	// Optional function for dialing out
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// buffer pool
	bufferPool bufferpool.BufPool
	// goroutine pool
	gPool GPool
	// user's handle
	userConnectHandle   func(ctx context.Context, writer io.Writer, request *Request) error
	userBindHandle      func(ctx context.Context, writer io.Writer, request *Request) error
	userAssociateHandle func(ctx context.Context, writer io.Writer, request *Request) error
	done                chan bool
	listen              net.Listener
	httpProxyBindAddr   string
}

// NewServer creates a new Server
func NewServer(opts ...Option) *Server {
	stdLogger := log.New(os.Stderr, "socksLogger", log.Ldate|log.Ltime)
	socksLogger := logger.NewLogger(stdLogger)
	srv := &Server{
		authMethods: []Authenticator{},
		bufferPool:  bufferpool.NewPool(32 * 1024),
		resolver:    DNSResolver{},
		rules:       NewPermitAll(),
		logger:      socksLogger,
		dial: func(ctx context.Context, net_, addr string) (net.Conn, error) {
			return net.Dial(net_, addr)
		},
	}

	for _, opt := range opts {
		opt(srv)
	}

	// Ensure we have at least one authentication method enabled
	if (len(srv.authMethods) == 0) && srv.credentials != nil {
		srv.authMethods = []Authenticator{&UserPassAuthenticator{srv.credentials}}
	}
	if len(srv.authMethods) == 0 {
		srv.authMethods = []Authenticator{&NoAuthAuthenticator{}}
	}

	return srv
}

// ListenAndServe is used to create a listener and serve on it
func (sf *Server) ListenAndServe(network, addr string) error {
	prx := goproxy.NewProxyHttpServer()
	prx.Verbose = true

	dialer, err := proxy.SOCKS5(network, addr, nil, proxy.Direct)

	if err != nil {
		return err
	}

	prx.Tr.Dial = dialer.Dial

	// find a random port and listen to it
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return err
	}

	sf.httpProxyBindAddr = listener.Addr().String()

	errorChan := make(chan error)

	go func() {
		err := http.Serve(listener, prx)
		if err != nil {
			errorChan <- err
			return
		}
	}()

	go func() {
		l, err := net.Listen(network, addr)
		sf.listen = l
		if err != nil {
			errorChan <- err
			return
		}
		errorChan <- sf.Serve()
	}()

	return <-errorChan
}

// Serve is used to serve internet from a listener
func (sf *Server) Serve() error {
	for {
		conn, err := sf.listen.Accept()
		if err != nil {
			select {
			case <-sf.done:
				sf.logger.Info("Shutting socks5 server done")
				return nil
			default:
				sf.logger.Errorf("Accept failed: %v", err)
				return err
			}
		}
		sf.goFunc(func() {
			if err := sf.ServeConn(conn); err != nil {
				sf.logger.Errorf("server: %v", err)
			}
		})
	}
}

func (sf *Server) Shutdown() error {
	go func() { sf.done <- true }() // Shutting down the socks5 proxy
	err := sf.listen.Close()
	if err != nil {
		return err
	}
	return nil
}

// ServeConn is used to serve a single connection.
func (sf *Server) ServeConn(conn net.Conn) error {
	defer conn.Close()

	bufConn := bufio.NewReader(conn)

	b, err := bufConn.Peek(1)
	if err != nil {
		return err
	}

	switch b[0] {
	case statute.VersionSocks5:
		return sf.handleSocksRequest(conn, bufConn)
	default:
		return sf.handleHTTPRequest(conn, bufConn)
	}
}

func (sf *Server) handleHTTPRequest(conn net.Conn, bufConn *bufio.Reader) error {
	dstConn, err := net.Dial(sf.listen.Addr().Network(), sf.httpProxyBindAddr)
	defer dstConn.Close()
	if err != nil {
		return err
	}
	errChan := make(chan error)
	go func() {
		_, err := io.Copy(dstConn, bufConn)
		if err != nil {
			errChan <- err
		}
	}()
	go func() {
		_, err := io.Copy(conn, dstConn)
		if err != nil {
			errChan <- err
		}
	}()

	return <-errChan
}

func (sf *Server) handleSocksRequest(conn net.Conn, bufConn *bufio.Reader) error {
	var authContext *AuthContext

	mr, err := statute.ParseMethodRequest(bufConn)
	if err != nil {
		return err
	}

	// Authenticate the connection
	userAddr := ""
	if conn.RemoteAddr() != nil {
		userAddr = conn.RemoteAddr().String()
	}
	authContext, err = sf.authenticate(conn, bufConn, userAddr, mr.Methods)
	if err != nil {
		return fmt.Errorf("failed to authenticate: %w", err)
	}

	// The client request detail
	request, err := ParseRequest(bufConn)
	if err != nil {
		if errors.Is(err, statute.ErrUnrecognizedAddrType) {
			if err := SendReply(conn, statute.RepAddrTypeNotSupported, nil); err != nil {
				return fmt.Errorf("failed to send reply %w", err)
			}
		}
		return fmt.Errorf("failed to read destination address, %w", err)
	}

	if request.Request.Command != statute.CommandConnect &&
		request.Request.Command != statute.CommandBind &&
		request.Request.Command != statute.CommandAssociate {
		if err := SendReply(conn, statute.RepCommandNotSupported, nil); err != nil {
			return fmt.Errorf("failed to send reply, %v", err)
		}
		return fmt.Errorf("unrecognized command[%d]", request.Request.Command)
	}

	request.AuthContext = authContext
	request.LocalAddr = conn.LocalAddr()
	request.RemoteAddr = conn.RemoteAddr()
	// Process the client request
	return sf.handleRequest(conn, request)
}

// authenticate is used to handle connection authentication
func (sf *Server) authenticate(conn io.Writer, bufConn io.Reader,
	userAddr string, methods []byte) (*AuthContext, error) {
	// Select a usable method
	for _, auth := range sf.authMethods {
		for _, method := range methods {
			if auth.GetCode() == method {
				return auth.Authenticate(bufConn, conn, userAddr)
			}
		}
	}
	// No usable method found
	conn.Write([]byte{statute.VersionSocks5, statute.MethodNoAcceptable}) //nolint: errcheck
	return nil, statute.ErrNoSupportedAuth
}

func (sf *Server) goFunc(f func()) {
	if sf.gPool == nil || sf.gPool.Submit(f) != nil {
		go f()
	}
}