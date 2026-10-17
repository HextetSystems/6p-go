package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/HextetSystems/6paster-go-client/paster"
)

const (
	defaultServer  = "https://paste.theos.cloud/"
	defaultTTL     = "604800"
	maximumBytes   = int64(307200)
	truncateMarker = "\n\n[Content truncated]\n"
)

var errTruncateNonUTF8 = errors.New("--truncate supports UTF-8 text only")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv, nil))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string, httpClient *http.Client) int {
	server := getenv("SIXPASTER_SERVER")
	if server == "" {
		server = defaultServer
	}

	flags := flag.NewFlagSet("6p", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: 6p [--server URL] [--ttl VALUE] [--truncate[=BYTES]] [FILE]")
		fmt.Fprintln(stderr, "Upload FILE, or standard input when FILE is omitted or '-'.")
		flags.PrintDefaults()
	}
	serverFlag := flags.String("server", server, "6paster server URL (or SIXPASTER_SERVER)")
	ttlFlag := flags.String("ttl", defaultTTL, "paste lifetime in seconds or as a Go duration")
	truncate := truncateValue{limit: maximumBytes}
	flags.Var(&truncate, "truncate", "truncate UTF-8 content to a byte limit (custom: --truncate=BYTES)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 1 {
		fmt.Fprintln(stderr, "6p: expected at most one file")
		flags.Usage()
		return 2
	}

	ttl, err := parseTTL(*ttlFlag)
	if err != nil {
		fmt.Fprintf(stderr, "6p: invalid TTL: %v\n", err)
		return 2
	}

	content := stdin
	var file *os.File
	var warnOnce sync.Once
	warnTooLarge := func() {
		warnOnce.Do(func() {
			fmt.Fprintf(stderr, "6p: warning: content exceeds %d bytes and may be rejected by the server\n", maximumBytes)
		})
	}
	if flags.NArg() == 1 && flags.Arg(0) != "-" {
		file, err = os.Open(flags.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "6p: open %q: %v\n", flags.Arg(0), err)
			return 1
		}
		defer file.Close()
		content = file
		if info, statErr := file.Stat(); statErr == nil && info.Size() > maximumBytes && (!truncate.enabled || truncate.limit > maximumBytes) {
			warnTooLarge()
		}
	}
	if truncate.enabled {
		content = newTruncatingReader(content, truncate.limit, nil)
	}
	content = &warningReader{reader: content, limit: maximumBytes, warn: warnTooLarge}

	client, err := paster.New(*serverFlag, httpClient)
	if err != nil {
		fmt.Fprintf(stderr, "6p: %v\n", err)
		return 2
	}
	pasteURL, err := client.Create(ctx, content, ttl)
	if err != nil {
		var httpErr *paster.HTTPError
		if errors.As(err, &httpErr) {
			fmt.Fprintf(stderr, "6p: %s\n", escapeTerminalControls(err.Error()))
		} else {
			fmt.Fprintf(stderr, "6p: %v\n", err)
		}
		return 1
	}
	if _, err := fmt.Fprintln(stdout, pasteURL.String()); err != nil {
		fmt.Fprintf(stderr, "6p: write result: %v\n", err)
		return 1
	}
	return 0
}

func escapeTerminalControls(value string) string {
	var escaped strings.Builder
	for _, r := range value {
		if unicode.IsPrint(r) {
			escaped.WriteRune(r)
			continue
		}
		quoted := strconv.QuoteRuneToGraphic(r)
		escaped.WriteString(quoted[1 : len(quoted)-1])
	}
	return escaped.String()
}

type truncateValue struct {
	enabled bool
	limit   int64
}

func (v *truncateValue) String() string {
	if !v.enabled {
		return "false"
	}
	return strconv.FormatInt(v.limit, 10)
}

func (v *truncateValue) Set(value string) error {
	switch value {
	case "true":
		v.enabled = true
		v.limit = maximumBytes
		return nil
	case "false":
		v.enabled = false
		v.limit = maximumBytes
		return nil
	}
	limit, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return errors.New("must be a positive decimal byte count")
	}
	if limit < int64(len(truncateMarker)) {
		return fmt.Errorf("must be at least %d bytes to fit the truncation marker", len(truncateMarker))
	}
	v.enabled = true
	v.limit = limit
	return nil
}

func (*truncateValue) IsBoolFlag() bool { return true }

type truncatingReader struct {
	source      *bufio.Reader
	limit       int64
	prefixLimit int64
	prefixBytes int64
	tail        []byte
	output      []byte
	inTail      bool
	done        bool
	terminalErr error
	onTruncate  func()
}

func newTruncatingReader(reader io.Reader, limit int64, onTruncate func()) io.Reader {
	return &truncatingReader{
		source:      bufio.NewReader(reader),
		limit:       limit,
		prefixLimit: limit - int64(len(truncateMarker)),
		tail:        make([]byte, 0, len(truncateMarker)+utf8.UTFMax),
		onTruncate:  onTruncate,
	}
}

func (r *truncatingReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.output) == 0 && !r.done && r.terminalErr == nil {
		r.fill(len(p))
	}
	if len(r.output) > 0 {
		n := copy(p, r.output)
		r.output = r.output[n:]
		return n, nil
	}
	if r.terminalErr != nil {
		err := r.terminalErr
		r.terminalErr = nil
		r.done = true
		return 0, err
	}
	return 0, io.EOF
}

func (r *truncatingReader) fill(target int) {
	for len(r.output) < target && !r.done && r.terminalErr == nil {
		runeValue, size, err := r.source.ReadRune()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if r.inTail {
					r.output = append(r.output, r.tail...)
					r.tail = nil
				}
				r.done = true
				return
			}
			r.terminalErr = err
			return
		}
		if runeValue == utf8.RuneError && size == 1 {
			r.terminalErr = errTruncateNonUTF8
			return
		}
		encoded := []byte(string(runeValue))
		if !r.inTail && r.prefixBytes+int64(size) <= r.prefixLimit {
			r.output = append(r.output, encoded...)
			r.prefixBytes += int64(size)
			continue
		}
		r.inTail = true
		r.tail = append(r.tail, encoded...)
		if r.prefixBytes+int64(len(r.tail)) > r.limit {
			r.output = append(r.output, truncateMarker...)
			r.tail = nil
			r.done = true
			if r.onTruncate != nil {
				r.onTruncate()
			}
		}
	}
}

type warningReader struct {
	reader io.Reader
	limit  int64
	read   int64
	warn   func()
}

func (r *warningReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += int64(n)
	if r.read > r.limit {
		r.warn()
	}
	return n, err
}

func parseTTL(value string) (time.Duration, error) {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0, errors.New("must be greater than zero")
		}
		const maximumSeconds = int64(^uint64(0)>>1) / int64(time.Second)
		if seconds > maximumSeconds {
			return 0, errors.New("value is too large")
		}
		return time.Duration(seconds) * time.Second, nil
	}

	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, errors.New("must be integer seconds or a duration such as 1h or 168h")
	}
	if duration <= 0 {
		return 0, errors.New("must be greater than zero")
	}
	if duration%time.Second != 0 {
		return 0, errors.New("must resolve to a whole number of seconds")
	}
	return duration, nil
}
