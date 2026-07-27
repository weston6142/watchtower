package proto

import (
	"bufio"
	"encoding/json"
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

func (c *Client) Close() error { return c.conn.Close() }
