package traycontrol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/privatefs"
)

const (
	SocketName      = "aplexicatray.sock"
	maxRequestBytes = 4096
	requestDeadline = 3 * time.Second
)

type Request struct {
	Command string `json:"command"`
}

type Response struct {
	OK      bool   `json:"ok"`
	Version string `json:"version,omitempty"`
	PID     int    `json:"pid,omitempty"`
	Error   string `json:"error,omitempty"`
}

type Server struct {
	socket   string
	version  string
	quit     func()
	mu       sync.Mutex
	listener net.Listener
}

func NewServer(socket, version string, quit func()) *Server {
	return &Server{socket: socket, version: version, quit: quit}
}

func (server *Server) Start() error {
	if err := privatefs.EnsureDir(filepath.Dir(server.socket), privatefs.DirPolicy{
		Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true,
	}); err != nil {
		return err
	}
	_ = os.Remove(server.socket)
	listener, err := net.Listen("unix", server.socket)
	if err != nil {
		return err
	}
	if err := privatefs.HardenOwnedPrivateSocket(server.socket); err != nil {
		_ = listener.Close()
		_ = os.Remove(server.socket)
		return err
	}
	server.mu.Lock()
	server.listener = listener
	server.mu.Unlock()
	go server.serve(listener)
	return nil
}

func (server *Server) Close() error {
	server.mu.Lock()
	listener := server.listener
	server.listener = nil
	server.mu.Unlock()
	if listener == nil {
		return nil
	}
	err := listener.Close()
	_ = os.Remove(server.socket)
	return err
}

func (server *Server) serve(listener net.Listener) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		go server.handle(connection)
	}
}

func (server *Server) handle(connection net.Conn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(requestDeadline))
	line, err := bufio.NewReader(io.LimitReader(connection, maxRequestBytes+1)).ReadBytes('\n')
	if err != nil || len(line) > maxRequestBytes {
		_ = json.NewEncoder(connection).Encode(Response{Error: "invalid request"})
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		_ = json.NewEncoder(connection).Encode(Response{Error: "invalid request"})
		return
	}
	switch request.Command {
	case "status":
		_ = json.NewEncoder(connection).Encode(Response{OK: true, Version: server.version, PID: os.Getpid()})
	case "quit-for-update":
		_ = json.NewEncoder(connection).Encode(Response{OK: true, Version: server.version, PID: os.Getpid()})
		if server.quit != nil {
			go server.quit()
		}
	default:
		_ = json.NewEncoder(connection).Encode(Response{Error: "unknown command"})
	}
}

func Send(ctx context.Context, socket, command string) (Response, error) {
	dialer := net.Dialer{Timeout: time.Second}
	connection, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return Response{}, err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(requestDeadline))
	request, err := json.Marshal(Request{Command: command})
	if err != nil {
		return Response{}, err
	}
	if _, err := connection.Write(append(request, '\n')); err != nil {
		return Response{}, err
	}
	line, err := bufio.NewReader(io.LimitReader(connection, maxRequestBytes+1)).ReadBytes('\n')
	if err != nil || len(line) > maxRequestBytes {
		return Response{}, fmt.Errorf("invalid tray response")
	}
	var response Response
	if err := json.Unmarshal(line, &response); err != nil {
		return Response{}, err
	}
	return response, nil
}
