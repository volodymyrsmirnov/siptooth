// Package hfp implements the Hands-Free Profile HF role over an RFCOMM stream (spec section 3.3).
package hfp

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// splitLines extracts complete lines (terminated by \r\n or \n) from buf, trimming
// surrounding \r and whitespace, dropping empty lines, and returning the unconsumed remainder.
func splitLines(buf []byte) ([]string, []byte) {
	var lines []string
	for {
		i := indexNewline(buf)
		if i < 0 {
			return lines, buf
		}
		line := strings.Trim(string(buf[:i]), "\r\n \t")
		buf = buf[i+1:]
		if line != "" {
			lines = append(lines, line)
		}
	}
}

func indexNewline(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return -1
}

func isFinal(line string) bool {
	switch {
	case line == "OK", line == "ERROR", line == "NO CARRIER", line == "BUSY", line == "NO ANSWER", line == "DELAYED", line == "BLACKLISTED":
		return true
	case strings.HasPrefix(line, "+CME ERROR:"):
		return true
	}
	return false
}

func afterColon(line, prefix string) (string, error) {
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("expected %s, got %q", prefix, line)
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if rest == "" {
		return "", fmt.Errorf("empty payload in %q", line)
	}
	return rest, nil
}

func parseBRSF(line string) (int, error) {
	rest, err := afterColon(line, "+BRSF:")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(rest)
}

// parseCINDList parses +CIND: ("name",(min,max)),... into name -> 1-based position.
func parseCINDList(line string) (map[string]int, error) {
	rest, err := afterColon(line, "+CIND:")
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	pos := 0
	for {
		start := strings.IndexByte(rest, '"')
		if start < 0 {
			break
		}
		end := strings.IndexByte(rest[start+1:], '"')
		if end < 0 {
			return nil, errors.New("unterminated indicator name")
		}
		pos++
		out[rest[start+1:start+1+end]] = pos
		rest = rest[start+1+end+1:]
		// skip the (min,max) range: find the closing "))" of this entry
		closeIdx := strings.Index(rest, "))")
		if closeIdx < 0 {
			return nil, errors.New("malformed indicator range")
		}
		rest = rest[closeIdx+2:]
	}
	if pos == 0 {
		return nil, errors.New("no indicators")
	}
	return out, nil
}

func parseCINDValues(line string) ([]int, error) {
	rest, err := afterColon(line, "+CIND:")
	if err != nil {
		return nil, err
	}
	parts := strings.Split(rest, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("bad indicator value %q", p)
		}
		out = append(out, v)
	}
	return out, nil
}

func parseCIEV(line string) (int, int, error) {
	rest, err := afterColon(line, "+CIEV:")
	if err != nil {
		return 0, 0, err
	}
	parts := strings.SplitN(rest, ",", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("bad +CIEV %q", line)
	}
	i, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	v, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("bad +CIEV %q", line)
	}
	return i, v, nil
}

// parseCLIP parses +CLIP: "<number>",<type>[,...]; the number may be empty or unquoted.
func parseCLIP(line string) (string, int, error) {
	rest, err := afterColon(line, "+CLIP:")
	if err != nil {
		return "", 0, err
	}
	parts := strings.Split(rest, ",")
	if len(parts) < 2 {
		return "", 0, fmt.Errorf("bad +CLIP %q", line)
	}
	number := strings.Trim(strings.TrimSpace(parts[0]), `"`)
	typ, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return "", 0, fmt.Errorf("bad +CLIP type in %q", line)
	}
	return number, typ, nil
}
