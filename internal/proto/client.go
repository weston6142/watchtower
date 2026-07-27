package proto

import (
	"bufio"
	"encoding/json"
	"net"
)

type Client struct {
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
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	return &Client{conn: conn, sc: sc, enc: json.NewEncoder(conn)}, nil
}

func (c *Client) Do(cmd Command) (Response, error) {
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
