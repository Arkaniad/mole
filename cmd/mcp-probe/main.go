// Command mcp-probe connects to a running mole daemon the way a coding agent's MCP
// client does, and prints what the agent would receive.
//
// A development tool, not part of the product: the in-process test rig covers tool
// behaviour, and this covers what the rig cannot — that Instructions actually
// traverse a real socket through the real shim, and what a fetched document looks
// like by the time it reaches a client.
//
//	go run ./cmd/mcp-probe /path/to/mole-mcp https://example.org/page
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.Command(os.Args[1]) // the mole-mcp shim
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "spike", Version: "0"}, nil).
		Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer cs.Close()

	fmt.Println("=== server instructions, as the client receives them")
	fmt.Println(cs.InitializeResult().Instructions)

	open, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "mole.session_open", Arguments: map[string]any{"question": "fasting and insulin"},
	})
	if err != nil || open.IsError {
		log.Fatalf("session_open: %v %+v", err, open)
	}
	var opened struct {
		SessionID string `json:"session_id"`
	}
	raw, _ := json.Marshal(open.StructuredContent)
	_ = json.Unmarshal(raw, &opened)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "mole.fetch",
		Arguments: map[string]any{"session_id": opened.SessionID, "url": os.Args[2]},
	})
	if err != nil {
		log.Fatalf("fetch: %v", err)
	}
	if res.IsError {
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				log.Fatalf("fetch error: %s", tc.Text)
			}
		}
		log.Fatalf("fetch error (no text content)")
	}
	out, _ := json.MarshalIndent(res.StructuredContent, "", " ")
	fmt.Println("\n=== mole.fetch result, as the client receives it")
	fmt.Println(string(out))
}
