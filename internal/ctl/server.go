package ctl

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// SocketPath is the Unix domain socket for pipoolctl communication.
// /run/pipool is created by systemd's RuntimeDirectory=pipool directive on every boot,
// so it survives reboots even though /run is a tmpfs.
const SocketPath = "/run/pipool/pipool.sock"

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
	// /run/pipool is created by systemd RuntimeDirectory= before this process starts.
	// When running outside systemd (e.g. dev), create it manually as a fallback.
	os.MkdirAll("/run/pipool", 0755)

	// Remove stale socket from a previous run
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
	case "calc":
		renderCalc(resp.Data)
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
	fmt.Fprintf(w, "COIN\tENABLED\tMINERS\tHASHRATE\tBLOCKS\tNODE\n")
	fmt.Fprintf(w, "────\t───────\t──────\t────────\t──────\t────\n")
	for _, c := range coins {
		fmt.Fprintf(w, "%v\t%v\t%v\t%v\t%v\t%v\n",
			c["symbol"], c["enabled"], c["miners"],
			c["hashrate"], c["blocks"], c["node"])
	}
	w.Flush()
}

func renderCalc(data interface{}) {
	b, _ := json.Marshal(data)
	var results []map[string]interface{}
	if err := json.Unmarshal(b, &results); err != nil || len(results) == 0 {
		fmt.Println("No data.")
		return
	}
	const powerball = 292201338.0
	const megamillions = 302575350.0
	fmtOdds := func(odds float64) string {
		switch {
		case odds < 100:
			return fmt.Sprintf("1 in %.1f", odds)
		case odds < 10000:
			return fmt.Sprintf("1 in %d", int(math.Round(odds)))
		case odds < 1e6:
			return fmt.Sprintf("1 in %.1fK", odds/1000)
		default:
			return fmt.Sprintf("1 in %.2fM", odds/1e6)
		}
	}
	fmtTime := func(sec float64) string {
		switch {
		case sec < 60:
			return fmt.Sprintf("%.0fs", sec)
		case sec < 3600:
			return fmt.Sprintf("%.1f min", sec/60)
		case sec < 86400:
			return fmt.Sprintf("%.1f hrs", sec/3600)
		default:
			return fmt.Sprintf("%.1f days", sec/86400)
		}
	}
	fmtKHs := func(khs float64) string {
		switch {
		case khs >= 1e9:
			return fmt.Sprintf("%.3f EH/s", khs/1e9)
		case khs >= 1e6:
			return fmt.Sprintf("%.3f PH/s", khs/1e6)
		case khs >= 1e3:
			return fmt.Sprintf("%.3f TH/s", khs/1e3)
		case khs >= 1:
			return fmt.Sprintf("%.3f GH/s", khs)
		case khs >= 0.001:
			return fmt.Sprintf("%.3f MH/s", khs*1000)
		default:
			return fmt.Sprintf("%.1f KH/s", khs*1e6)
		}
	}
	fmt.Println()
	fmt.Println("  ┌─────────────────────────────────────────────────────────────────────┐")
	fmt.Println("  │                    SOLO MINING LUCK CALCULATOR                      │")
	fmt.Println("  └─────────────────────────────────────────────────────────────────────┘")
	fmt.Println()
	for _, r := range results {
		sym, _ := r["symbol"].(string)
		algo, _ := r["algorithm"].(string)
		khs, _ := r["hashrate_khs"].(float64)
		diff, _ := r["network_diff"].(float64)
		expSec, _ := r["expected_sec"].(float64)
		oddsHr, _ := r["odds_per_hour"].(float64)
		pctHr, _ := r["pct_per_hour"].(float64)
		vsPb, _ := r["vs_powerball"].(float64)
		fmt.Printf("  %-6s  %s\n", sym, strings.ToUpper(algo))
		fmt.Printf("  ─────────────────────────────────────\n")
		fmt.Printf("  Hashrate:       %s\n", fmtKHs(khs))
		fmt.Printf("  Network Diff:   %.4g\n", diff)
		fmt.Printf("  Expected Time:  %s\n", fmtTime(expSec))
		fmt.Printf("  Block Odds/hr:  %s  (%.4f%% chance)\n", fmtOdds(oddsHr), pctHr)
		// Comparison
		if vsPb >= 1 {
			x := vsPb
			var xs string
			switch {
			case x >= 1e6:
				xs = fmt.Sprintf("%.1fM×", x/1e6)
			case x >= 1e3:
				xs = fmt.Sprintf("%dK×", int(math.Round(x/1000)))
			default:
				xs = fmt.Sprintf("%d×", int(math.Round(x)))
			}
			fmt.Printf("  vs Powerball:   %s BETTER than jackpot (1 in 292M per draw)\n", xs)
		} else {
			x := 1 / vsPb
			var xs string
			switch {
			case x >= 1e6:
				xs = fmt.Sprintf("%.1fM×", x/1e6)
			case x >= 1e3:
				xs = fmt.Sprintf("%dK×", int(math.Round(x/1000)))
			default:
				xs = fmt.Sprintf("%d×", int(math.Round(x)))
			}
			fmt.Printf("  vs Powerball:   %s WORSE than jackpot (1 in 292M per draw)\n", xs)
		}
		vsMm := megamillions / oddsHr
		if vsMm >= 1 {
			x := vsMm
			var xs string
			switch {
			case x >= 1e6:
				xs = fmt.Sprintf("%.1fM×", x/1e6)
			case x >= 1e3:
				xs = fmt.Sprintf("%dK×", int(math.Round(x/1000)))
			default:
				xs = fmt.Sprintf("%d×", int(math.Round(x)))
			}
			fmt.Printf("  vs Mega Mill:   %s BETTER than jackpot (1 in 302M per draw)\n", xs)
		}
		// Probability breakdown
		pctDay := (1 - math.Exp(-86400/expSec)) * 100
		pctWeek := (1 - math.Exp(-7*86400/expSec)) * 100
		pctMonth := (1 - math.Exp(-30*86400/expSec)) * 100
		fmt.Printf("\n  Probability of finding at least 1 block:\n")
		fmt.Printf("    Per hour:   %.4f%%\n", pctHr)
		fmt.Printf("    Per day:    %.2f%%\n", pctDay)
		fmt.Printf("    Per week:   %.2f%%\n", pctWeek)
		fmt.Printf("    Per month:  %.2f%%\n", pctMonth)
		fmt.Println()
	}
}

func printHelp() {
	fmt.Print(`pipoolctl — PiPool runtime control

Usage: pipoolctl <command> [args]

Commands:
  status                        Show pool status (uptime, hashrate, temp, RAM)
  workers                       List all connected workers with device info
  coins                         Show per-coin stats (hashrate, blocks, PC node)
  coin enable  <SYMBOL>         Enable a coin  (e.g. pipoolctl coin enable PEP)
  coin disable <SYMBOL>         Disable a coin without restarting
  kick <worker>                 Disconnect a specific worker by name
  vardiff <SYMBOL> <min> <max>  Adjust vardiff bounds for a coin live
  reload                        Hot-reload pipool.json (discord, logging, limits)
  restart                       Gracefully restart PiPool (systemd brings it back)
  stop                          Stop PiPool (systemd will restart it automatically)
  loglevel <level>              Set log level: debug | info | warn | error
  discord test                  Send a test Discord notification
  version                       Show PiPool version
  calc [SYMBOL]                 Solo luck calculator — odds vs Powerball, expected time, probability per hr/day/week/month

Examples:
  pipoolctl status
  pipoolctl workers
  pipoolctl restart
  pipoolctl coin enable PEP
  pipoolctl vardiff LTC 0.001 2048
  pipoolctl kick myworker.1
  pipoolctl loglevel debug
  pipoolctl reload
`)
}
