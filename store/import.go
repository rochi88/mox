package store

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mjl-/mox/mlog"
)

// MsgSource is implemented by readers for mailbox file formats.
type MsgSource interface {
	// Return next message, or io.EOF when there are no more.
	Next() (*Message, *os.File, string, error)
}

// MboxReader reads messages from an mbox file, implementing MsgSource.
type MboxReader struct {
	createTemp func(pattern string) (*os.File, error)
	path       string
	line       int
	r          *bufio.Reader
	prevempty  bool
	nonfirst   bool
	log        *mlog.Log
	eof        bool
	fromLine   string // "From "-line for this message.
	header     bool   // Now in header section.
}

func NewMboxReader(createTemp func(pattern string) (*os.File, error), filename string, r io.Reader, log *mlog.Log) *MboxReader {
	return &MboxReader{
		createTemp: createTemp,
		path:       filename,
		line:       1,
		r:          bufio.NewReader(r),
		log:        log,
	}
}

// Position returns "<filename>:<lineno>" for the current position.
func (mr *MboxReader) Position() string {
	return fmt.Sprintf("%s:%d", mr.path, mr.line)
}

// Next returns the next message read from the mbox file. The file is a temporary
// file and must be removed/consumed. The third return value is the position in the
// file.
func (mr *MboxReader) Next() (*Message, *os.File, string, error) {
	if mr.eof {
		return nil, nil, "", io.EOF
	}

	from := []byte("From ")

	if !mr.nonfirst {
		mr.header = true
		// First read, we're at the beginning of the file.
		line, err := mr.r.ReadBytes('\n')
		if err == io.EOF {
			return nil, nil, "", io.EOF
		}
		mr.line++

		if !bytes.HasPrefix(line, from) {
			return nil, nil, mr.Position(), fmt.Errorf(`first line does not start with "From "`)
		}
		mr.nonfirst = true
		mr.fromLine = strings.TrimSpace(string(line))
	}

	f, err := mr.createTemp("mboxreader")
	if err != nil {
		return nil, nil, mr.Position(), err
	}
	defer func() {
		if f != nil {
			err := os.Remove(f.Name())
			mr.log.Check(err, "removing temporary message file after mbox read error", mlog.Field("path", f.Name()))
			err = f.Close()
			mr.log.Check(err, "closing temporary message file after mbox read error")
		}
	}()

	fromLine := mr.fromLine
	bf := bufio.NewWriter(f)
	var flags Flags
	var size int64
	for {
		line, err := mr.r.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, nil, mr.Position(), fmt.Errorf("reading from mbox: %v", err)
		}
		if len(line) > 0 {
			mr.line++
			// We store data with crlf, adjust any imported messages with bare newlines.
			if !bytes.HasSuffix(line, []byte("\r\n")) {
				line = append(line[:len(line)-1], "\r\n"...)
			}

			if mr.header {
				// See https://doc.dovecot.org/admin_manual/mailbox_formats/mbox/
				if bytes.HasPrefix(line, []byte("Status:")) {
					s := strings.TrimSpace(strings.SplitN(string(line), ":", 2)[1])
					for _, c := range s {
						switch c {
						case 'R':
							flags.Seen = true
						}
					}
				} else if bytes.HasPrefix(line, []byte("X-Status:")) {
					s := strings.TrimSpace(strings.SplitN(string(line), ":", 2)[1])
					for _, c := range s {
						switch c {
						case 'A':
							flags.Answered = true
						case 'F':
							flags.Flagged = true
						case 'T':
							flags.Draft = true
						case 'D':
							flags.Deleted = true
						}
					}
				} else if bytes.HasPrefix(line, []byte("X-Keywords:")) {
					s := strings.TrimSpace(strings.SplitN(string(line), ":", 2)[1])
					for _, t := range strings.Split(s, ",") {
						flagSet(&flags, strings.ToLower(strings.TrimSpace(t)))
					}
				}
			}
			if bytes.Equal(line, []byte("\r\n")) {
				mr.header = false
			}

			// Next mail message starts at bare From word.
			if mr.prevempty && bytes.HasPrefix(line, from) {
				mr.fromLine = strings.TrimSpace(string(line))
				mr.header = true
				break
			}
			if bytes.HasPrefix(line, []byte(">")) && bytes.HasPrefix(bytes.TrimLeft(line, ">"), []byte("From ")) {
				line = line[1:]
			}
			n, err := bf.Write(line)
			if err != nil {
				return nil, nil, mr.Position(), fmt.Errorf("writing message to file: %v", err)
			}
			size += int64(n)
			mr.prevempty = bytes.Equal(line, []byte("\r\n"))
		}
		if err == io.EOF {
			mr.eof = true
			break
		}
	}
	if err := bf.Flush(); err != nil {
		return nil, nil, mr.Position(), fmt.Errorf("flush: %v", err)
	}

	m := &Message{Flags: flags, Size: size}

	if t := strings.SplitN(fromLine, " ", 3); len(t) == 3 {
		layouts := []string{time.ANSIC, time.UnixDate, time.RubyDate}
		for _, l := range layouts {
			t, err := time.Parse(l, t[2])
			if err == nil {
				m.Received = t
				break
			}
		}
	}

	// Prevent cleanup by defer.
	mf := f
	f = nil

	return m, mf, mr.Position(), nil
}

type MaildirReader struct {
	createTemp      func(pattern string) (*os.File, error)
	newf, curf      *os.File
	f               *os.File // File we are currently reading from. We first read newf, then curf.
	dir             string   // Name of directory for f. Can be empty on first call.
	entries         []os.DirEntry
	dovecotKeywords []string
	log             *mlog.Log
}

func NewMaildirReader(createTemp func(pattern string) (*os.File, error), newf, curf *os.File, log *mlog.Log) *MaildirReader {
	mr := &MaildirReader{
		createTemp: createTemp,
		newf:       newf,
		curf:       curf,
		f:          newf,
		log:        log,
	}

	// Best-effort parsing of dovecot keywords.
	kf, err := os.Open(filepath.Join(filepath.Dir(newf.Name()), "dovecot-keywords"))
	if err == nil {
		mr.dovecotKeywords, err = ParseDovecotKeywords(kf, log)
		log.Check(err, "parsing dovecot keywords file")
		err = kf.Close()
		log.Check(err, "closing dovecot-keywords file")
	}

	return mr
}

func (mr *MaildirReader) Next() (*Message, *os.File, string, error) {
	if mr.dir == "" {
		mr.dir = mr.f.Name()
	}

	if len(mr.entries) == 0 {
		var err error
		mr.entries, err = mr.f.ReadDir(100)
		if err != nil && err != io.EOF {
			return nil, nil, "", err
		}
		if len(mr.entries) == 0 {
			if mr.f == mr.curf {
				return nil, nil, "", io.EOF
			}
			mr.f = mr.curf
			mr.dir = ""
			return mr.Next()
		}
	}

	p := filepath.Join(mr.dir, mr.entries[0].Name())
	mr.entries = mr.entries[1:]
	sf, err := os.Open(p)
	if err != nil {
		return nil, nil, p, fmt.Errorf("open message in maildir: %s", err)
	}
	defer func() {
		err := sf.Close()
		mr.log.Check(err, "closing message file after error")
	}()
	f, err := mr.createTemp("maildirreader")
	if err != nil {
		return nil, nil, p, err
	}
	defer func() {
		if f != nil {
			err := os.Remove(f.Name())
			mr.log.Check(err, "removing temporary message file after maildir read error", mlog.Field("path", f.Name()))
			err = f.Close()
			mr.log.Check(err, "closing temporary message file after maildir read error")
		}
	}()

	// Copy data, changing bare \n into \r\n.
	r := bufio.NewReader(sf)
	w := bufio.NewWriter(f)
	var size int64
	for {
		line, err := r.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, nil, p, fmt.Errorf("reading message: %v", err)
		}
		if len(line) > 0 {
			if !bytes.HasSuffix(line, []byte("\r\n")) {
				line = append(line[:len(line)-1], "\r\n"...)
			}

			if n, err := w.Write(line); err != nil {
				return nil, nil, p, fmt.Errorf("writing message: %v", err)
			} else {
				size += int64(n)
			}
		}
		if err == io.EOF {
			break
		}
	}
	if err := w.Flush(); err != nil {
		return nil, nil, p, fmt.Errorf("writing message: %v", err)
	}

	// Take received time from filename.
	var received time.Time
	t := strings.SplitN(filepath.Base(sf.Name()), ".", 2)
	if v, err := strconv.ParseInt(t[0], 10, 64); err == nil {
		received = time.Unix(v, 0)
	}

	// Parse flags. See https://cr.yp.to/proto/maildir.html.
	flags := Flags{}
	t = strings.SplitN(filepath.Base(sf.Name()), ":2,", 2)
	if len(t) == 2 {
		for _, c := range t[1] {
			switch c {
			case 'P':
				// Passed, doesn't map to a common IMAP flag.
			case 'R':
				flags.Answered = true
			case 'S':
				flags.Seen = true
			case 'T':
				flags.Deleted = true
			case 'D':
				flags.Draft = true
			case 'F':
				flags.Flagged = true
			default:
				if c >= 'a' && c <= 'z' {
					index := int(c - 'a')
					if index >= len(mr.dovecotKeywords) {
						continue
					}
					kw := mr.dovecotKeywords[index]
					switch kw {
					case "$Forwarded", "Forwarded":
						flags.Forwarded = true
					case "$Junk", "Junk":
						flags.Junk = true
					case "$NotJunk", "NotJunk", "NonJunk":
						flags.Notjunk = true
					case "$MDNSent":
						flags.MDNSent = true
					case "$Phishing", "Phishing":
						flags.Phishing = true
					}
					// todo: custom labels, e.g. $label1, JunkRecorded?
				}
			}
		}
	}

	m := &Message{Received: received, Flags: flags, Size: size}

	// Prevent cleanup by defer.
	mf := f
	f = nil

	return m, mf, p, nil
}

func ParseDovecotKeywords(r io.Reader, log *mlog.Log) ([]string, error) {
	/*
		If the dovecot-keywords file is present, we parse its additional flags, see
		https://doc.dovecot.org/admin_manual/mailbox_formats/maildir/

		0 Old
		1 Junk
		2 NonJunk
		3 $Forwarded
		4 $Junk
	*/
	keywords := make([]string, 26)
	end := 0
	scanner := bufio.NewScanner(r)
	var errs []string
	for scanner.Scan() {
		s := scanner.Text()
		t := strings.SplitN(s, " ", 2)
		if len(t) != 2 {
			errs = append(errs, fmt.Sprintf("unexpected dovecot keyword line: %q", s))
			continue
		}
		v, err := strconv.ParseInt(t[0], 10, 32)
		if err != nil {
			errs = append(errs, fmt.Sprintf("unexpected dovecot keyword index: %q", s))
			continue
		}
		if v < 0 || v >= int64(len(keywords)) {
			errs = append(errs, fmt.Sprintf("dovecot keyword index too big: %q", s))
			continue
		}
		index := int(v)
		if keywords[index] != "" {
			errs = append(errs, fmt.Sprintf("duplicate dovecot keyword: %q", s))
			continue
		}
		keywords[index] = t[1]
		if index >= end {
			end = index + 1
		}
	}
	if err := scanner.Err(); err != nil {
		errs = append(errs, fmt.Sprintf("reading dovecot keywords file: %v", err))
	}
	var err error
	if len(errs) > 0 {
		err = errors.New(strings.Join(errs, "; "))
	}
	return keywords[:end], err
}

func flagSet(flags *Flags, word string) {
	switch word {
	case "forwarded", "$forwarded":
		flags.Forwarded = true
	case "junk", "$junk":
		flags.Junk = true
	case "notjunk", "$notjunk", "nonjunk", "$nonjunk":
		flags.Notjunk = true
	case "phishing", "$phishing":
		flags.Phishing = true
	case "mdnsent", "$mdnsent":
		flags.MDNSent = true
	}
}
