package handler

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/korotovsky/slack-mcp-server/pkg/test/util"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestIntegrationPersonalChannelSlackOnlyLoop(t *testing.T) {
	channelID := os.Getenv("SLACK_MCP_PERSONAL_TEST_CHANNEL")
	if channelID == "" {
		t.Skip("set SLACK_MCP_PERSONAL_TEST_CHANNEL to a personal Slack DM/channel ID to run this posting smoke test")
	}
	if os.Getenv("SLACK_MCP_XOXP_TOKEN") == "" &&
		os.Getenv("SLACK_MCP_XOXB_TOKEN") == "" &&
		(os.Getenv("SLACK_MCP_XOXC_TOKEN") == "" || os.Getenv("SLACK_MCP_XOXD_TOKEN") == "") {
		t.Skip("set Slack auth env vars before running the personal channel smoke test")
	}

	sseKey := uuid.New().String()
	mcpServer, err := util.SetupMCP(util.MCPConfig{
		SSEKey:             sseKey,
		MessageToolEnabled: true,
		MessageToolPolicy:  channelID,
		FileUploadPolicy:   channelID,
		EnabledTools: []string{
			"conversations_add_message",
			"conversations_replies",
			"files_upload",
		},
	})
	require.NoError(t, err)
	defer mcpServer.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	mcpClient, err := client.NewSSEMCPClient(
		fmt.Sprintf("http://%s:%d/sse", mcpServer.Host, mcpServer.Port),
		client.WithHeaders(map[string]string{"Authorization": "Bearer " + sseKey}),
	)
	require.NoError(t, err)
	require.NoError(t, mcpClient.Start(ctx))
	defer mcpClient.Close()

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "personal-channel-smoke-test", Version: "1.0.0"}
	_, err = mcpClient.Initialize(ctx, initReq)
	require.NoError(t, err)

	messageText := "Slack MCP personal-channel smoke test: raw Block Kit post"
	messageResult := callSmokeTool(t, ctx, mcpClient, "conversations_add_message", map[string]any{
		"channel_id": channelID,
		"text":       messageText,
		"blocks": []any{
			map[string]any{
				"type": "section",
				"text": map[string]any{
					"type": "mrkdwn",
					"text": "*" + messageText + "*",
				},
			},
			map[string]any{
				"type": "context",
				"elements": []any{
					map[string]any{"type": "mrkdwn", "text": "`conversations_add_message` raw blocks path"},
				},
			},
		},
	})
	messageTS := firstCSVValue(t, messageResult, "MsgID")
	require.NotEmpty(t, messageTS)

	uploadResult := callSmokeTool(t, ctx, mcpClient, "files_upload", map[string]any{
		"channel_id":      channelID,
		"thread_ts":       messageTS,
		"filename":        "slack-mcp-personal-smoke-test.txt",
		"title":           "Slack MCP personal smoke test",
		"initial_comment": "Slack MCP personal-channel smoke test: files_upload with base64 content.",
		"content_base64":  base64.StdEncoding.EncodeToString([]byte("Slack MCP personal-channel smoke test\n")),
	})

	var upload struct {
		FileID  string `json:"file_id"`
		Channel string `json:"channel_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(toolText(t, uploadResult)), &upload))
	require.NotEmpty(t, upload.FileID)
	require.Equal(t, channelID, upload.Channel)

	repliesResult := callSmokeTool(t, ctx, mcpClient, "conversations_replies", map[string]any{
		"channel_id": channelID,
		"thread_ts":  messageTS,
		"limit":      "10",
	})
	require.Contains(t, toolText(t, repliesResult), upload.FileID)
}

func callSmokeTool(t *testing.T, ctx context.Context, c *client.Client, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()

	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	result, err := c.CallTool(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError, "tool %s returned error: %s", name, toolText(t, result))
	return result
}

func toolText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()

	var out strings.Builder
	for _, content := range result.Content {
		if textContent, ok := content.(mcp.TextContent); ok {
			out.WriteString(textContent.Text)
		}
	}
	return out.String()
}

func firstCSVValue(t *testing.T, result *mcp.CallToolResult, field string) string {
	t.Helper()

	rows, err := csv.NewReader(strings.NewReader(toolText(t, result))).ReadAll()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(rows), 2)

	for i, name := range rows[0] {
		if name == field {
			require.Less(t, i, len(rows[1]))
			return rows[1][i]
		}
	}
	t.Fatalf("CSV field %q not found in header %q", field, strings.Join(rows[0], ","))
	return ""
}
