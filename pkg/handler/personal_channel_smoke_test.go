package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
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

	messageText := "Slack MCP personal-channel smoke test: text-only post"
	messageResult := callSmokeTool(t, ctx, mcpClient, "conversations_add_message", map[string]any{
		"channel_id":   channelID,
		"text":         messageText,
		"content_type": "text/plain",
	})
	messageTS := firstCSVValue(t, messageResult, "MsgID")
	require.NotEmpty(t, messageTS)

	callSmokeTool(t, ctx, mcpClient, "conversations_add_message", map[string]any{
		"channel_id": channelID,
		"thread_ts":  messageTS,
		"blocks": []any{
			map[string]any{
				"type": "section",
				"text": map[string]any{
					"type": "mrkdwn",
					"text": "*Slack MCP personal-channel smoke test: blocks-only post*",
				},
			},
			map[string]any{
				"type": "context",
				"elements": []any{
					map[string]any{"type": "mrkdwn", "text": "`conversations_add_message` raw blocks-only path"},
				},
			},
		},
	})

	mixedText := "Slack MCP personal-channel smoke test: mixed text and raw blocks post"
	callSmokeTool(t, ctx, mcpClient, "conversations_add_message", map[string]any{
		"channel_id": channelID,
		"thread_ts":  messageTS,
		"text":       mixedText,
		"blocks": []any{
			map[string]any{
				"type": "section",
				"text": map[string]any{
					"type": "mrkdwn",
					"text": "*" + mixedText + "*",
				},
			},
			map[string]any{
				"type": "context",
				"elements": []any{
					map[string]any{"type": "mrkdwn", "text": "`conversations_add_message` raw blocks plus fallback text path"},
				},
			},
		},
	})

	textUpload := parseUploadResult(t, callSmokeTool(t, ctx, mcpClient, "files_upload", map[string]any{
		"channel_id":      channelID,
		"thread_ts":       messageTS,
		"filename":        "slack-mcp-personal-smoke-test.txt",
		"title":           "Slack MCP personal smoke test",
		"initial_comment": "Slack MCP personal-channel smoke test: files_upload with base64 content.",
		"content_base64":  base64.StdEncoding.EncodeToString([]byte("Slack MCP personal-channel smoke test\n")),
	}))
	require.Equal(t, channelID, textUpload.Channel)

	imageUpload := parseUploadResult(t, callSmokeTool(t, ctx, mcpClient, "files_upload", map[string]any{
		"channel_id":      channelID,
		"thread_ts":       messageTS,
		"filename":        "slack-mcp-personal-smoke-test.png",
		"title":           "Slack MCP personal smoke test image",
		"initial_comment": "Slack MCP personal-channel smoke test: PNG image upload.",
		"content_base64":  smokeTestPNGBase64(t),
	}))
	require.Equal(t, channelID, imageUpload.Channel)

	repliesText := waitForThreadFiles(t, ctx, mcpClient, channelID, messageTS, textUpload.FileID, imageUpload.FileID)
	require.Contains(t, repliesText, textUpload.FileID)
	require.Contains(t, repliesText, imageUpload.FileID)
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

func parseUploadResult(t *testing.T, result *mcp.CallToolResult) struct {
	FileID  string `json:"file_id"`
	Channel string `json:"channel_id"`
} {
	t.Helper()

	var upload struct {
		FileID  string `json:"file_id"`
		Channel string `json:"channel_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(toolText(t, result)), &upload))
	require.NotEmpty(t, upload.FileID)
	return upload
}

func smokeTestPNGBase64(t *testing.T) string {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 320, 180))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{R: 245, G: 247, B: 250, A: 255}}, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(0, 0, 320, 48), &image.Uniform{C: color.RGBA{R: 24, G: 144, B: 219, A: 255}}, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(28, 78, 292, 132), &image.Uniform{C: color.RGBA{R: 36, G: 43, B: 54, A: 255}}, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(40, 92, 102, 118), &image.Uniform{C: color.RGBA{R: 255, G: 255, B: 255, A: 255}}, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(118, 92, 280, 118), &image.Uniform{C: color.RGBA{R: 96, G: 211, B: 148, A: 255}}, image.Point{}, draw.Src)

	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func waitForThreadFiles(t *testing.T, ctx context.Context, c *client.Client, channelID, threadTS string, fileIDs ...string) string {
	t.Helper()

	var repliesText string
	require.Eventually(t, func() bool {
		repliesResult := callSmokeTool(t, ctx, c, "conversations_replies", map[string]any{
			"channel_id": channelID,
			"thread_ts":  threadTS,
			"limit":      "20",
		})
		repliesText = toolText(t, repliesResult)
		for _, fileID := range fileIDs {
			if !strings.Contains(repliesText, fileID) {
				return false
			}
		}
		return true
	}, 30*time.Second, 2*time.Second)

	return repliesText
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
