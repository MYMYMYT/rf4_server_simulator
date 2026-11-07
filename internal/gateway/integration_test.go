package gateway_test

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"rf4server/internal/proto"
	"rf4server/internal/testutil"
)

// TestGatewayPlainHTTPCompatLogin 旧版 CN 客户端会向 7515 发明文 HTTP，
// 且把上游 URL 原样放进 /lv54/https://... 路径。兼容口必须保留双斜杠、
// 不经 ServeMux 清理或重定向，直接返回旧版 <response> XML。
func TestGatewayPlainHTTPCompatLogin(t *testing.T) {
	_, stubAddr := testutil.StartStubGameServer(t, testutil.TestPSK)
	gw := testutil.StartGateway(t, stubAddr, testutil.TestPSK)
	if gw.LoginPlainURL == "" {
		t.Fatal("明文 HTTP 登录兼容口没有启动")
	}
	realClientURL := strings.TrimSuffix(gw.LoginPlainURL, "/login.php") +
		"/lv54/https://api.rf4game.ru/login.php?login=return2&version=4.0.25034&region=CN&locale=zh-CN"
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(realClientURL)
	if err != nil {
		t.Fatalf("明文登录请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !bytes.Contains(body, []byte(`<response success="true">`)) ||
		!bytes.Contains(body, []byte(`<token>`)) {
		t.Fatalf("真实客户端路径必须直接返回 200，不能重定向: status=%d location=%q body=%s",
			resp.StatusCode, resp.Header.Get("Location"), body)
	}
	text := string(body)
	start := strings.Index(text, `<token>`) + len(`<token>`)
	end := strings.Index(text[start:], `</token>`)
	if start < len(`<token>`) || end < 0 || len(text[start:start+end]) != 32 || strings.ContainsAny(text[start:start+end], `|/`) {
		t.Fatalf("兼容 token 必须是 32hex 且不能含 URI 分隔符: %q", text[start:start+end])
	}
	legacyToken := text[start : start+end]

	// 旧版客户端随后在 19213 回传这个 32hex token。认证包总长 38 字节，
	// 服务端必须立即回 36 字节 UUID，否则客户端报 invalid response length 0/36。
	conn, err := net.DialTimeout("tcp", gw.RealAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("连接 realtime 失败: %v", err)
	}
	defer conn.Close()
	auth := append([]byte{proto.AuthMagic, 0x00}, proto.PackU32(uint32(len(legacyToken)))...)
	auth = append(auth, legacyToken...)
	if _, err := conn.Write(auth); err != nil {
		t.Fatalf("发送兼容认证包失败: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	greeting := make([]byte, 36)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		t.Fatalf("读取 realtime 问候失败: %v", err)
	}
	if string(greeting) != proto.GreetingUUID {
		t.Fatalf("realtime 问候不符: %q", greeting)
	}
}

// TestGatewayBadToken 无效 token 拒绝（连接关闭且无问候）。
func TestGatewayBadToken(t *testing.T) {
	_, stubAddr := testutil.StartStubGameServer(t, testutil.TestPSK)
	gw := testutil.StartGateway(t, stubAddr, testutil.TestPSK)

	bad := "dead|beef|cafe|babe"
	conn, err := net.DialTimeout("tcp", gw.RealAddr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	auth := append([]byte{proto.AuthMagic, 0x00}, proto.PackU32(uint32(len(bad)))...)
	auth = append(auth, bad...)
	if _, err := conn.Write(auth); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	if n, err := conn.Read(buf); err == nil && n >= 36 {
		t.Fatalf("无效 token 不应收到问候, 收到 %d 字节", n)
	}
}
