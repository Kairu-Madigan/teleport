/*
Copyright 2015 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package web

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/net/websocket"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/unicode"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/client"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/sshutils"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gravitational/trace"

	"github.com/sirupsen/logrus"
)

// TerminalRequest describes a request to create a web-based terminal
// to a remote SSH server.
type TerminalRequest struct {
	// Server describes a server to connect to (serverId|hostname[:port]).
	Server string `json:"server_id"`

	// Login is Linux username to connect as.
	Login string `json:"login"`

	// Term is the initial PTY size.
	Term session.TerminalParams `json:"term"`

	// SessionID is a Teleport session ID to join as.
	SessionID session.ID `json:"sid"`

	// Namespace is node namespace.
	Namespace string `json:"namespace"`

	// ProxyHostPort is the address of the server to connect to.
	ProxyHostPort string `json:"-"`

	// Cluster is the name of the remote cluster to connect to.
	Cluster string `json:"-"`

	// InteractiveCommand is a command to execut.e
	InteractiveCommand []string `json:"-"`

	// SessionTimeout is how long to wait for the session end event to arrive.
	SessionTimeout time.Duration
}

// AuthProvider is a subset of the full Auth API.
type AuthProvider interface {
	GetNodes(namespace string) ([]services.Server, error)
	GetSessionEvents(namespace string, sid session.ID, after int, includePrintEvents bool) ([]events.EventFields, error)
}

// newTerminal creates a web-based terminal based on WebSockets and returns a
// new TerminalHandler.
func NewTerminal(req TerminalRequest, authProvider AuthProvider, ctx *SessionContext) (*TerminalHandler, error) {
	if req.SessionTimeout == 0 {
		req.SessionTimeout = defaults.HTTPIdleTimeout
	}

	// Make sure whatever session is requested is a valid session.
	_, err := session.ParseID(string(req.SessionID))
	if err != nil {
		return nil, trace.BadParameter("sid: invalid session id")
	}

	if req.Login == "" {
		return nil, trace.BadParameter("login: missing login")
	}
	if req.Term.W <= 0 || req.Term.H <= 0 {
		return nil, trace.BadParameter("term: bad term dimensions")
	}

	servers, err := authProvider.GetNodes(req.Namespace)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	hostName, hostPort, err := resolveServerHostPort(req.Server, servers)
	if err != nil {
		return nil, trace.BadParameter("invalid server name %q: %v", req.Server, err)
	}

	return &TerminalHandler{
		log: logrus.WithFields(logrus.Fields{
			trace.Component: teleport.ComponentWebsocket,
		}),
		namespace:      req.Namespace,
		sessionID:      req.SessionID,
		params:         req,
		ctx:            ctx,
		hostName:       hostName,
		hostPort:       hostPort,
		authProvider:   authProvider,
		sessionTimeout: req.SessionTimeout,
		encoder:        unicode.UTF8.NewEncoder(),
		decoder:        unicode.UTF8.NewDecoder(),
	}, nil
}

// TerminalHandler connects together an SSH session with a web-based
// terminal via a web socket.
type TerminalHandler struct {
	// log holds the structured logger.
	log *logrus.Entry

	// namespace is node namespace.
	namespace string

	// sessionID is a Teleport session ID to join as.
	sessionID session.ID

	// params is the initial PTY size.
	params TerminalRequest

	// ctx is a web session context for the currently logged in user.
	ctx *SessionContext

	// ws is the websocket which is connected to stdin/out/err of the terminal shell.
	ws *websocket.Conn

	// hostName is the hostname of the server.
	hostName string

	// hostPort is the port of the server.
	hostPort int

	// sshSession holds the "shell" SSH channel to the node.
	sshSession *ssh.Session

	// teleportClient is the client used to form the connection.
	teleportClient *client.TeleportClient

	// terminalContext is used to signal when the terminal sesson is closing.
	terminalContext context.Context

	// terminalCancel is used to signal when the terminal session is closing.
	terminalCancel context.CancelFunc

	// request is the HTTP request that initiated the websocket connection.
	request *http.Request

	// authProvider is used to fetch nodes and sessions from the backend.
	authProvider AuthProvider

	// sessionTimeout is how long to wait for the session end event to arrive.
	sessionTimeout time.Duration

	// encoder is used to encode strings into UTF-8.
	encoder *encoding.Encoder

	// decoder is used to decode UTF-8 strings.
	decoder *encoding.Decoder
}

// Serve builds a connect to the remote node and then pumps back two types of
// events: raw input/output events for what's happening on the terminal itself
// and audit log events relevant to this session.
func (t *TerminalHandler) Serve(w http.ResponseWriter, r *http.Request) {
	t.request = r

	// This allows closing of the websocket if the user logs out before exiting
	// the session.
	t.ctx.AddClosers(t)
	defer t.ctx.RemoveCloser(t)

	// We initial a server explicitly here instead of using websocket.HandlerFunc
	// to set an empty origin checker (this is to make our lives easier in tests).
	// The main use of the origin checker is to enforce the browsers same-origin
	// policy. That does not matter here because even if malicious Javascript
	// would try and open a websocket the request to this endpoint requires the
	// bearer token to be in the URL so it would not be sent along by default
	// like cookies are.
	ws := &websocket.Server{Handler: t.handler}
	ws.ServeHTTP(w, r)
}

// Close the websocket stream.
func (t *TerminalHandler) Close() error {
	// Close the websocket connection to the client web browser.
	if t.ws != nil {
		t.ws.Close()
	}

	// Close the SSH connection to the remote node.
	if t.sshSession != nil {
		t.sshSession.Close()
	}

	// If the terminal handler was closed (most likely due to the *SessionContext
	// closing) then the stream should be closed as well.
	t.terminalCancel()

	return nil
}

// handler is the main websocket loop. It creates a Teleport client and then
// pumps raw events and audit events back to the client until the SSH session
// is complete.
func (t *TerminalHandler) handler(ws *websocket.Conn) {
	// Create a Teleport client, if not able to, show the reason to the user in
	// the terminal.
	tc, err := t.makeClient(ws)
	if err != nil {
		er := t.errToTerm(err, ws)
		if er != nil {
			t.log.Warnf("Unable to send error to terminal: %v: %v.", err, er)
		}
		return
	}

	// Create a context for signaling when the terminal session is over.
	t.terminalContext, t.terminalCancel = context.WithCancel(context.Background())

	t.log.Debugf("Creating websocket stream for %v.", t.sessionID)

	// Pump raw terminal in/out and audit events into the websocket.
	go t.streamTerminal(ws, tc)
	go t.streamEvents(ws, tc)

	// Block until the terminal session is complete.
	<-t.terminalContext.Done()
	t.log.Debugf("Closing websocket stream for %v.", t.sessionID)
}

// makeClient builds a *client.TeleportClient for the connection.
func (t *TerminalHandler) makeClient(ws *websocket.Conn) (*client.TeleportClient, error) {
	agent, cert, err := t.ctx.GetAgent()
	if err != nil {
		return nil, trace.BadParameter("failed to get user credentials: %v", err)
	}

	signers, err := agent.Signers()
	if err != nil {
		return nil, trace.BadParameter("failed to get user credentials: %v", err)
	}

	tlsConfig, err := t.ctx.ClientTLSConfig()
	if err != nil {
		return nil, trace.BadParameter("failed to get client TLS config: %v", err)
	}

	// Create a wrapped websocket to wrap/unwrap the envelope used to
	// communicate over the websocket.
	wrappedSock := newWrappedSocket(ws, t)

	clientConfig := &client.Config{
		SkipLocalAuth:    true,
		ForwardAgent:     true,
		Agent:            agent,
		TLS:              tlsConfig,
		AuthMethods:      []ssh.AuthMethod{ssh.PublicKeys(signers...)},
		DefaultPrincipal: cert.ValidPrincipals[0],
		HostLogin:        t.params.Login,
		Username:         t.ctx.user,
		Namespace:        t.params.Namespace,
		Stdout:           wrappedSock,
		Stderr:           wrappedSock,
		Stdin:            wrappedSock,
		SiteName:         t.params.Cluster,
		ProxyHostPort:    t.params.ProxyHostPort,
		Host:             t.hostName,
		HostPort:         t.hostPort,
		Env:              map[string]string{sshutils.SessionEnvVar: string(t.params.SessionID)},
		HostKeyCallback:  func(string, net.Addr, ssh.PublicKey) error { return nil },
		ClientAddr:       t.request.RemoteAddr,
	}
	if len(t.params.InteractiveCommand) > 0 {
		clientConfig.Interactive = true
	}

	tc, err := client.NewClient(clientConfig)
	if err != nil {
		return nil, trace.BadParameter("failed to create client: %v", err)
	}

	// Save the *ssh.Session after the shell has been created. The session is
	// used to update all other parties window size to that of the web client and
	// to allow future window changes.
	tc.OnShellCreated = func(s *ssh.Session, c *ssh.Client, _ io.ReadWriteCloser) (bool, error) {
		t.sshSession = s
		t.windowChange(&t.params.Term)
		return false, nil
	}

	return tc, nil
}

// streamTerminal opens a SSH connection to the remote host and streams
// events back to the web client.
func (t *TerminalHandler) streamTerminal(ws *websocket.Conn, tc *client.TeleportClient) {
	defer t.terminalCancel()

	// Establish SSH connection to the server. This function will block until
	// either an error occurs or it completes successfully.
	err := tc.SSH(t.terminalContext, t.params.InteractiveCommand, false)
	if err != nil {
		t.log.Warnf("Unable to stream terminal: %v.", err)
		er := t.errToTerm(err, ws)
		if er != nil {
			t.log.Warnf("Unable to send error to terminal: %v: %v.", err, er)
		}
		return
	}

	// Send close envelope to web terminal upon exit without an error.
	err = websocket.Message.Send(ws, defaults.CloseWebsocketPrefix)
	if err != nil {
		t.log.Errorf("Unable to send close event to web client.")
		return
	}
	t.log.Debugf("Sent close event to web client.")
}

// streamEvents receives events over the SSH connection and forwards them to
// the web client.
func (t *TerminalHandler) streamEvents(ws *websocket.Conn, tc *client.TeleportClient) {
	for {
		select {
		// Send push events that come over the events channel to the web client.
		case event := <-tc.EventsChannel():
			data, err := json.Marshal(event)
			if err != nil {
				t.log.Errorf("Unable to marshal audit event %v: %v.", event.GetType(), err)
				continue
			}

			t.log.Debugf("Sending audit event %v to web client.", event.GetType())

			encoded, err := t.encoder.String(defaults.AuditWebsocketPrefix + string(data))
			err = websocket.Message.Send(ws, encoded)
			if err != nil {
				t.log.Errorf("Unable to send audit event %v to web client: %v.", event.GetType(), err)
				continue
			}
		// Once the terminal stream is over (and the close envelope has been sent),
		// close stop streaming envelopes.
		case <-t.terminalContext.Done():
			return
		}
	}
}

// windowChange is called when the browser window is resized. It sends a
// "window-change" channel request to the server.
func (t *TerminalHandler) windowChange(params *session.TerminalParams) error {
	if t.sshSession == nil {
		return nil
	}

	_, err := t.sshSession.SendRequest(
		sshutils.WindowChangeRequest,
		false,
		ssh.Marshal(sshutils.WinChangeReqParams{
			W: uint32(params.W),
			H: uint32(params.H),
		}))
	if err != nil {
		t.log.Error(err)
	}

	return trace.Wrap(err)
}

// errToTerm displays an error in the terminal window.
func (t *TerminalHandler) errToTerm(err error, w io.Writer) error {
	// Replace \n with \r\n so the message correctly aligned.
	r := strings.NewReplacer("\r\n", "\r\n", "\n", "\r\n")
	errMessage := r.Replace(err.Error())

	encoded, err := t.encoder.String(defaults.RawWebsocketPrefix + errMessage)
	if err != nil {
		return trace.Wrap(err)
	}

	// Write the error to the websocket.
	_, err = w.Write([]byte(encoded))
	if err != nil {
		return trace.Wrap(err)
	}

	return nil
}

// resolveServerHostPort parses server name and attempts to resolve hostname
// and port.
func resolveServerHostPort(servername string, existingServers []services.Server) (string, int, error) {
	// If port is 0, client wants us to figure out which port to use.
	var defaultPort = 0

	if servername == "" {
		return "", defaultPort, trace.BadParameter("empty server name")
	}

	// Check if servername is UUID.
	for i := range existingServers {
		node := existingServers[i]
		if node.GetName() == servername {
			return node.GetHostname(), defaultPort, nil
		}
	}

	if !strings.Contains(servername, ":") {
		return servername, defaultPort, nil
	}

	// Check for explicitly specified port.
	host, portString, err := utils.SplitHostPort(servername)
	if err != nil {
		return "", defaultPort, trace.Wrap(err)
	}

	port, err := strconv.Atoi(portString)
	if err != nil {
		return "", defaultPort, trace.BadParameter("invalid port: %v", err)
	}

	return host, port, nil
}

// wrappedSocket wraps and unwraps the envelope that is used to send events
// over the websocket.
type wrappedSocket struct {
	ws       *websocket.Conn
	terminal *TerminalHandler

	encoder *encoding.Encoder
	decoder *encoding.Decoder
}

func newWrappedSocket(ws *websocket.Conn, terminal *TerminalHandler) *wrappedSocket {
	if ws == nil {
		return nil
	}
	return &wrappedSocket{
		ws:       ws,
		terminal: terminal,
		encoder:  unicode.UTF8.NewEncoder(),
		decoder:  unicode.UTF8.NewDecoder(),
	}
}

// Write wraps the data bytes in a raw envelope and sends.
func (w *wrappedSocket) Write(data []byte) (n int, err error) {
	encoded, err := w.encoder.String(defaults.RawWebsocketPrefix + string(data))
	if err != nil {
		return 0, trace.Wrap(err)
	}

	err = websocket.Message.Send(w.ws, encoded)
	if err != nil {
		return 0, trace.Wrap(err)
	}

	return len(data), nil
}

// Read unwraps the envelope and either fills out the passed in bytes or
// performs an action on the connection (sending window-change request).
func (w *wrappedSocket) Read(out []byte) (n int, err error) {
	var str string
	err = websocket.Message.Receive(w.ws, &str)
	if err != nil {
		if err == io.EOF {
			return 0, io.EOF
		}
		return 0, trace.Wrap(err)
	}

	var data []byte
	data, err = w.decoder.Bytes([]byte(str))
	if err != nil {
		return 0, trace.Wrap(err)
	}

	if len(data) < 1 {
		return 0, trace.BadParameter("frame must have length of at least 1")
	}

	switch string(data[0]) {
	case defaults.RawWebsocketPrefix:
		if len(out) < len(data[1:]) {
			if w.terminal != nil {
				w.terminal.log.Warnf("websocket failed to receive everything: %d vs %d", len(out), len(data))
			}
		}
		return copy(out, data[1:]), nil
	case defaults.ResizeWebsocketPrefix:
		if w.terminal == nil {
			return 0, nil
		}

		var e events.EventFields
		err := json.Unmarshal(data[1:], &e)
		if err != nil {
			return 0, trace.Wrap(err)
		}

		params, err := session.UnmarshalTerminalParams(e.GetString("size"))
		if err != nil {
			return 0, trace.Wrap(err)
		}

		// Send the window change request in a goroutine so reads are not blocked
		// by network connectivity issues.
		go w.terminal.windowChange(params)

		return 0, nil
	default:
		return 0, trace.BadParameter("unknown prefix type: %v", string(data[0]))
	}
}

// SetReadDeadline sets the network read deadline on the underlying websocket.
func (w *wrappedSocket) SetReadDeadline(t time.Time) error {
	return w.ws.SetReadDeadline(t)
}

// Close the websocket.
func (w *wrappedSocket) Close() error {
	return w.ws.Close()
}

// eventEnvelope is used to send/receive audit events.
type eventEnvelope struct {
	Payload events.EventFields `json:"p"`
}
