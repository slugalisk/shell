package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/golang/protobuf/ptypes"
	"github.com/slugalisk/shell/proto/go"

	"google.golang.org/grpc"
)

var host string
var port int
var reconnectIvl int64

func init() {
	flag.StringVar(&host, "host", "localhost", "command server host")
	flag.IntVar(&port, "port", 30013, "command server tcp port")
	flag.Int64Var(&reconnectIvl, "reconnect-ivl", 10, "reconnect interval in seconds")
}

// SendOutput ...
func SendOutput(client shell.Shell_FollowClient, commandID string, source shell.CommandOutput_Source, line string) {
	time, _ := ptypes.TimestampProto(time.Now())
	client.Send(&shell.FollowRequest{
		Data: &shell.FollowRequest_Output{
			Output: &shell.CommandOutput{
				CommandId: commandID,
				Time:      time,
				Source:    source,
				Line:      line,
			},
		},
	})
}

// SendExit ...
func SendExit(client shell.Shell_FollowClient, commandID string, code int64) {
	time, _ := ptypes.TimestampProto(time.Now())
	client.Send(&shell.FollowRequest{
		Data: &shell.FollowRequest_Exit{
			Exit: &shell.CommandExit{
				CommandId: commandID,
				Time:      time,
				Code:      0,
			},
		},
	})
}

// Shell client wrapper
type Shell struct {
	client shell.ShellClient
}

// NewShell create client wrapper
func NewShell(host string, port int) *Shell {
	conn, _ := grpc.Dial(net.JoinHostPort(host, strconv.Itoa(port)), grpc.WithInsecure())
	return &Shell{
		client: shell.NewShellClient(conn),
	}
}

// Follow connect to the server and await commands
func (s *Shell) Follow() error {
	client, err := s.client.Follow(context.Background())
	if err != nil {
		return err
	}
	log.Println("connected")

	for {
		req, err := client.Recv()
		if err == io.EOF {
			return fmt.Errorf("server disconnected")
		}
		if err != nil {
			return err
		}

		go func() {
			if err := s.exec(req.Command, client); err != nil {
				SendOutput(client, req.Command.Id, shell.CommandOutput_DAEMON, err.Error())
				SendExit(client, req.Command.Id, 1)
			} else {
				SendExit(client, req.Command.Id, 0)
			}
		}()
	}
}

// FollowForever ...
func (s *Shell) FollowForever(reconnectIvl int64) {
	for {
		log.Println(s.Follow())
		time.Sleep(time.Duration(reconnectIvl) * time.Second)
	}
}

// pumpResponse ships results from a io.ReadCloser to the command server one line at a time
func (s *Shell) pumpResponse(
	commandID string,
	source shell.CommandOutput_Source,
	pipe io.ReadCloser,
	client shell.Shell_FollowClient,
	wg *sync.WaitGroup,
) {
	scanner := bufio.NewScanner(pipe)
	for scanner.Scan() {
		SendOutput(client, commandID, source, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		SendOutput(client, commandID, shell.CommandOutput_DAEMON, err.Error())
	}

	wg.Done()
}

func (s *Shell) exec(command *shell.Command, client shell.Shell_FollowClient) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(command.Timeout)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, command.Name, command.Args...)

	// set up buffers for stderr/stdout
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	// run the command
	if err := cmd.Start(); err != nil {
		return err
	}

	// pump the results to the client
	wg := &sync.WaitGroup{}
	wg.Add(2)
	go s.pumpResponse(command.Id, shell.CommandOutput_STDOUT, stdout, client, wg)
	go s.pumpResponse(command.Id, shell.CommandOutput_STDERR, stderr, client, wg)
	wg.Wait()

	// wait for the command to exit and send an error (probably non 0 exit code...?)
	if err := cmd.Wait(); err != nil {
		return err
	}

	return nil
}

func main() {
	flag.Parse()

	shell := NewShell(host, port)
	shell.FollowForever(reconnectIvl)
}