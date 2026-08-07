package proto

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"
)

type Client struct {
	// mu serializes request/response pairs: the TUI issues concurrent Do
	// calls (tail + overview + issue_detail per tick) over one connection,
	// and interleaved frames corrupt the JSONL stream.
	mu   sync.Mutex
	conn net.Conn
	sc   *bufio.Scanner
	enc  *json.Encoder
}

func Dial(sockPath string) (*Client, error) {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, maxMessageBytes), maxMessageBytes)
	return &Client{conn: conn, sc: sc, enc: json.NewEncoder(conn)}, nil
}

func (c *Client) Do(cmd Command) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cmd.Op == "tail" {
		return c.doTailLocked(cmd)
	}
	return c.doLocked(cmd)
}

func (c *Client) doLocked(cmd Command) (Response, error) {
	if err := c.enc.Encode(cmd); err != nil {
		return Response{}, err
	}
	if !c.sc.Scan() {
		return Response{}, c.sc.Err()
	}
	var r Response
	if err := json.Unmarshal(c.sc.Bytes(), &r); err != nil {
		return Response{}, err
	}
	return r, nil
}

func (c *Client) doTailLocked(cmd Command) (Response, error) {
	combined := Response{OK: true, ThroughSeq: cmd.ThroughSeq}
	for {
		page, err := c.doLocked(cmd)
		if err != nil {
			return Response{}, err
		}
		if !page.OK {
			return page, nil
		}
		if cmd.ThroughSeq == 0 {
			cmd.ThroughSeq = page.ThroughSeq
			combined.ThroughSeq = page.ThroughSeq
		} else if page.ThroughSeq != cmd.ThroughSeq {
			return Response{}, fmt.Errorf("tail replay boundary changed from %d to %d", cmd.ThroughSeq, page.ThroughSeq)
		}
		combined.Events = append(combined.Events, page.Events...)
		if cmd.ThroughSeq == 0 || cmd.SinceSeq >= cmd.ThroughSeq {
			return combined, nil
		}
		if len(page.Events) == 0 {
			return Response{}, fmt.Errorf("tail replay stopped at %d before boundary %d", cmd.SinceSeq, cmd.ThroughSeq)
		}
		nextSeq := page.Events[len(page.Events)-1].Seq
		if nextSeq <= cmd.SinceSeq {
			return Response{}, fmt.Errorf("tail replay did not advance after sequence %d", cmd.SinceSeq)
		}
		if nextSeq > cmd.ThroughSeq {
			return Response{}, fmt.Errorf("tail replay advanced past boundary %d to %d", cmd.ThroughSeq, nextSeq)
		}
		cmd.SinceSeq = nextSeq
		if cmd.SinceSeq == cmd.ThroughSeq {
			return combined, nil
		}
	}
}

func (c *Client) Close() error { return c.conn.Close() }
