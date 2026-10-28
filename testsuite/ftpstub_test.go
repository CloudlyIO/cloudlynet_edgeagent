package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"
)

// stubFTP is a minimal in-process FTP control/data server, just enough of
// RFC 959 for the commands the mock device's real curl sends (login, EPSV/PASV,
// NLST, STOR, DELE, SIZE/MDTM/REST for a no-body request). It lets the curl
// command lines in ftp.go be exercised end to end without a vsftpd container.
type stubFTP struct {
	t     *testing.T
	ln    net.Listener
	mu    sync.Mutex
	files map[string][]byte
	cmds  []string
}

func newStubFTP(t *testing.T, names ...string) *stubFTP {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not on PATH")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("stub ftp listen: %v", err)
	}
	s := &stubFTP{t: t, ln: ln, files: map[string][]byte{}}
	for _, n := range names {
		s.files[n] = []byte("x")
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *stubFTP) host() string { return s.ln.Addr().String() }

func (s *stubFTP) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.files))
	for n := range s.files {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (s *stubFTP) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cmds...)
}

func (s *stubFTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	reply := func(format string, a ...any) { fmt.Fprintf(c, format+"\r\n", a...) }
	reply("220 stub ftp")
	var data net.Listener
	defer func() {
		if data != nil {
			data.Close()
		}
	}()
	openData := func() (net.Listener, error) {
		if data != nil {
			data.Close()
		}
		var err error
		data, err = net.Listen("tcp", "127.0.0.1:0")
		return data, err
	}
	accept := func() (net.Conn, bool) {
		if data == nil {
			reply("425 no data connection")
			return nil, false
		}
		dc, err := data.Accept()
		if err != nil {
			reply("425 data accept failed")
			return nil, false
		}
		return dc, true
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd, arg, _ := strings.Cut(line, " ")
		cmd = strings.ToUpper(cmd)
		s.mu.Lock()
		s.cmds = append(s.cmds, line)
		s.mu.Unlock()
		name := strings.TrimPrefix(arg, "/")
		switch cmd {
		case "USER":
			reply("331 password please")
		case "PASS":
			reply("230 logged in")
		case "PWD":
			reply(`257 "/"`)
		case "CWD":
			reply("250 ok")
		case "TYPE":
			reply("200 ok")
		case "EPSV":
			l, err := openData()
			if err != nil {
				reply("425 cannot open data port")
				continue
			}
			reply("229 Entering Extended Passive Mode (|||%d|)", l.Addr().(*net.TCPAddr).Port)
		case "PASV":
			l, err := openData()
			if err != nil {
				reply("425 cannot open data port")
				continue
			}
			p := l.Addr().(*net.TCPAddr).Port
			reply("227 Entering Passive Mode (127,0,0,1,%d,%d)", p/256, p%256)
		case "NLST", "LIST":
			dc, ok := accept()
			if !ok {
				continue
			}
			reply("150 listing")
			for _, n := range s.names() {
				fmt.Fprintf(dc, "%s\r\n", n)
			}
			dc.Close()
			reply("226 done")
		case "STOR":
			dc, ok := accept()
			if !ok {
				continue
			}
			reply("150 send it")
			b, _ := io.ReadAll(dc)
			dc.Close()
			s.mu.Lock()
			s.files[name] = b
			s.mu.Unlock()
			reply("226 stored")
		case "DELE":
			s.mu.Lock()
			_, ok := s.files[name]
			delete(s.files, name)
			s.mu.Unlock()
			if ok {
				reply("250 deleted")
			} else {
				reply("550 no such file")
			}
		case "SIZE":
			s.mu.Lock()
			b, ok := s.files[name]
			s.mu.Unlock()
			if ok {
				reply("213 %d", len(b))
			} else {
				reply("550 no such file")
			}
		case "MDTM":
			reply("213 20261027120000")
		case "REST":
			reply("350 ok")
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("502 not implemented")
		}
	}
}
