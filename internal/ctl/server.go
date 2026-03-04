package ctl

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

const SocketPath = "/var/run/pipool/pipool.sock"

// ─── Command definitions ──────────────────────────────────────────────────────

type Command struct {
	Cmd  string `json:"cmd"`
	Args []string `json:"args,omitempty"`
}

type Response struct {
	OK      bool        `json:"ok"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

// ─── Pool-side control server ─────────────────────────────────────────────────

// Handler is a function that handles a control command and returns a Response
type Handler func(args []string) Response

// Server listens on the Unix socket and dispatches commands to handlers
type Server struct {
	handlers map[string]Handler
	ln       net.Listener
}

// NewServer creates a new control server
func NewServer() *Server {
	return &Server{handlers: make(map[string]Handler)}
}

// Register adds a command handler
func (s *Server) Register(cmd string, h Handler) {
	s.handlers[cmd] = h
}

// Start begins listening on the Unix socket
func (s *Server) Start() error {
	// Ensure socket directory exists
	if err := os.MkdirAll("/var/run/pipool", 0750); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	// Remove stale socket if present
	os.Remove(SocketPath)

	ln, err := net.Listen("unix", SocketPath)
	if err != nil {
		return fmt.Errorf("listen unix socket: %w", err)
	}
	if err := os.Chmod(SocketPath, 0660); err != nil {
		return fmt.Errorf("chmod socket: %w", err)
	}

	s.ln = ln
	go s.accept()
	return nil
}

func (s *Server) Stop() {
	if s.ln != nil {
		s.ln.Close()
	}
	os.Remove(SocketPath)
}

func (s *Server) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		var cmd Command
		if err := json.Unmarshal(scanner.Bytes(), &cmd); err != nil {
			writeResp(conn, Response{OK: false, Message: "bad JSON"})
			continue
		}

		h, ok := s.handlers[strings.ToLower(cmd.Cmd)]
		if !ok {
			writeResp(conn, Response{OK: false, Message: fmt.Sprintf("unknown command: %s", cmd.Cmd)})
			continue
		}

		resp := h(cmd.Args)
		writeResp(conn, resp)
	}
}

func writeResp(conn net.Conn, r Response) {
	data, _ := json.Marshal(r)
	conn.Write(append(data, '\n'))
}

// ─── Client-side CLI (used by pipoolctl binary) ───────────────────────────────

// RunCLI is the entry point for the pipoolctl command-line tool.
// It connects to the pool's Unix socket and sends a command.
func RunCLI(args []string) {
	if len(args) == 0 {
		printHelp()
		os.Exit(0)
	}

	conn, err := net.Dial("unix", SocketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot connect to PiPool socket at %s\n", SocketPath)
		fmt.Fprintf(os.Stderr, "Is pipool running? (sudo systemctl status pipool)\n")
		os.Exit(1)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	cmd := Command{Cmd: args[0], Args: args[1:]}
	data, _ := json.Marshal(cmd)
	conn.Write(append(data, '\n'))

	scanner := bufio.NewScanner(conn)
	if scanner.Scan() {
		var resp Response
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			fmt.Println(scanner.Text())
			return
		}
		renderResponse(cmd.Cmd, resp)
	}
}

func renderResponse(cmd string, resp Response) {
	if !resp.OK {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp.Message)
		os.Exit(1)
	}

	switch cmd {
	case "status":
		renderStatus(resp.Data)
	case "workers":
		renderWorkers(resp.Data)
	case "coins":
		renderCoins(resp.Data)
	default:
		if resp.Message != "" {
			fmt.Println(resp.Message)
		} else if resp.Data != nil {
			b, _ := json.MarshalIndent(resp.Data, "", "  ")
			fmt.Println(string(b))
		} else {
			fmt.Println("OK")
		}
	}
}

func renderStatus(data interface{}) {
	b, _ := json.Marshal(data)
	var m map[string]interface{}
	json.Unmarshal(b, &m)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "PiPool Status\n")
	fmt.Fprintf(w, "─────────────────────────────\n")
	for k, v := range m {
		fmt.Fprintf(w, "%s\t%v\n", k, v)
	}
	w.Flush()
}

func renderWorkers(data interface{}) {
	b, _ := json.Marshal(data)
	var workers []map[string]interface{}
	json.Unmarshal(b, &workers)

	if len(workers) == 0 {
		fmt.Println("No workers connected.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "WORKER\tCOIN\tDEVICE\tDIFF\tSHARES\tADDRESS\n")
	fmt.Fprintf(w, "──────\t────\t──────\t────\t──────\t───────\n")
	for _, worker := range workers {
		fmt.Fprintf(w, "%v\t%v\t%v\t%v\t%v\t%v\n",
			worker["name"], worker["coin"], worker["device"],
			worker["diff"], worker["shares"], worker["addr"])
	}
	w.Flush()
}

func renderCoins(data interface{}) {
	b, _ := json.Marshal(data)
	var coins []map[string]interface{}
	json.Unmarshal(b, &coins)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "COIN\tENABLED\tMINERS\tHASHRATE\tBLOCKS\tHEIGHT\n")
	fmt.Fprintf(w, "────\t───────\t──────\t────────\t──────\t──────\n")
	for _, c := range coins {
		fmt.Fprintf(w, "%v\t%v\t%v\t%v\t%v\t%v\n",
			c["symbol"], c["enabled"], c["miners"],
			c["hashrate"], c["blocks"], c["height"])
	}
	w.Flush()
}

func printHelp() {
	fmt.Print(`pipoolctl — PiPool runtime control

Usage: pipoolctl <command> [args]

Commands:
  status                   Show pool status (uptime, hashrate, temp, RAM)
  workers                  List all connected workers with device info
  coins                    Show per-coin stats
  coin enable  <SYMBOL>    Enable a coin (e.g. pipoolctl coin enable PEP)
  coin disable <SYMBOL>    Disable a coin without restarting
  kick <worker>            Disconnect a specific worker by name
  vardiff <SYMBOL> <min> <max>  Adjust vardiff bounds for a coin live
  reload                   Hot-reload pipool.json config (wallets, discord, etc.)
  loglevel <level>         Set log level: debug | info | warn | error
  discord test             Send a test Discord notification
  version                  Show PiPool version

Examples:
  pipoolctl status
  pipoolctl workers
  pipoolctl coin enable PEP
  pipoolctl vardiff LTC 0.001 2048
  pipoolctl kick myworker.1
  pipoolctl loglevel debug
  pipoolctl reload
`)
}
