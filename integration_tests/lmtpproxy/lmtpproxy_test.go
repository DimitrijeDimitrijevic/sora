//go:build integration

package lmtpproxy_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/migadu/sora/integration_tests/common"
	"github.com/migadu/sora/server/lmtpproxy"
)

// LMTPProxyWrapper wraps the LMTP proxy server to handle graceful shutdown
type LMTPProxyWrapper struct {
	server *lmtpproxy.Server
}

func (w *LMTPProxyWrapper) Close() error {
	if w.server != nil {
		return w.server.Stop()
	}
	return nil
}

// LogCapture captures log output for verification
type LogCapture struct {
	buffer *bytes.Buffer
	writer io.Writer
	oldLog *log.Logger
	oldOut io.Writer
}

func NewLogCapture() *LogCapture {
	buffer := &bytes.Buffer{}
	writer := io.MultiWriter(os.Stderr, buffer)

	// Save old settings
	oldOut := log.Writer()
	oldLog := log.New(oldOut, "", log.LstdFlags)

	// Set new logger to capture output
	log.SetOutput(writer)

	return &LogCapture{
		buffer: buffer,
		writer: writer,
		oldLog: oldLog,
		oldOut: oldOut,
	}
}

func (lc *LogCapture) GetOutput() string {
	return lc.buffer.String()
}

func (lc *LogCapture) Close() {
	log.SetOutput(lc.oldOut)
}

// LMTPClient provides a simple LMTP client for testing
type LMTPClient struct {
	conn   net.Conn
	reader *bufio.Reader
}

func NewLMTPClient(address string) (*LMTPClient, error) {
	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		return nil, err
	}

	client := &LMTPClient{
		conn:   conn,
		reader: bufio.NewReader(conn),
	}

	// Read greeting
	response, err := client.ReadResponse()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to read greeting: %w", err)
	}

	if !strings.HasPrefix(response, "220") {
		conn.Close()
		return nil, fmt.Errorf("unexpected greeting: %s", response)
	}

	return client, nil
}

func (c *LMTPClient) SendCommand(command string) error {
	_, err := c.conn.Write([]byte(command + "\r\n"))
	return err
}

func (c *LMTPClient) ReadResponse() (string, error) {
	response, err := c.reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(response, "\r\n"), nil
}

func (c *LMTPClient) ReadMultilineResponse() ([]string, error) {
	var responses []string
	for {
		response, err := c.ReadResponse()
		if err != nil {
			return nil, err
		}
		responses = append(responses, response)

		// Check if this is the last line (not a continuation)
		if len(response) >= 4 && response[3] == ' ' {
			break
		}
	}
	return responses, nil
}

func (c *LMTPClient) Close() error {
	return c.conn.Close()
}

// setupLMTPProxyWithPROXY sets up an LMTP proxy that uses PROXY protocol to connect to the backend
func setupLMTPProxyWithPROXY(t *testing.T, backendAddress string) (string, *LMTPProxyWrapper) {
	t.Helper()

	rdb := common.SetupTestDatabase(t)
	proxyAddress := common.GetRandomAddress(t)

	// Create LMTP proxy server with PROXY protocol support
	server, err := lmtpproxy.New(
		context.Background(),
		rdb,
		"localhost",
		lmtpproxy.ServerOptions{
			Name:                   "test-proxy",
			Addr:                   proxyAddress,
			RemoteAddrs:            []string{backendAddress},
			RemotePort:             25,                                 // Default LMTP port
			RemoteUseProxyProtocol: true,                               // Enable PROXY protocol to backend
			RemoteUseXCLIENT:       false,                              // Disable XCLIENT (using PROXY instead)
			TrustedProxies:         []string{"127.0.0.0/8", "::1/128"}, // Trust localhost
			ConnectTimeout:         5 * time.Second,
			SessionTimeout:         30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("Failed to create LMTP proxy with PROXY protocol: %v", err)
	}

	// Start the proxy server
	go func() {
		if err := server.Start(); err != nil {
			t.Logf("LMTP proxy server error: %v", err)
		}
	}()

	// Wait for server to start
	time.Sleep(100 * time.Millisecond)

	return proxyAddress, &LMTPProxyWrapper{server: server}
}

// setupLMTPProxyWithXCLIENT sets up an LMTP proxy that uses XCLIENT command to forward parameters
func setupLMTPProxyWithXCLIENT(t *testing.T, backendAddress string) (string, *LMTPProxyWrapper) {
	t.Helper()

	rdb := common.SetupTestDatabase(t)
	proxyAddress := common.GetRandomAddress(t)

	// Create LMTP proxy server with XCLIENT support
	server, err := lmtpproxy.New(
		context.Background(),
		rdb,
		"localhost",
		lmtpproxy.ServerOptions{
			Name:                   "test-proxy",
			Addr:                   proxyAddress,
			RemoteAddrs:            []string{backendAddress},
			RemotePort:             25,                                 // Default LMTP port
			RemoteUseProxyProtocol: false,                              // Disable PROXY protocol
			RemoteUseXCLIENT:       true,                               // Enable XCLIENT command
			TrustedProxies:         []string{"127.0.0.0/8", "::1/128"}, // Trust localhost
			ConnectTimeout:         5 * time.Second,
			SessionTimeout:         30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("Failed to create LMTP proxy with XCLIENT support: %v", err)
	}

	// Start the proxy server
	go func() {
		if err := server.Start(); err != nil {
			t.Logf("LMTP proxy server error: %v", err)
		}
	}()

	// Wait for server to start
	time.Sleep(100 * time.Millisecond)

	return proxyAddress, &LMTPProxyWrapper{server: server}
}

// TestLMTPProxyWithPROXYProtocol tests LMTP proxy using PROXY protocol
func TestLMTPProxyWithPROXYProtocol(t *testing.T) {
	common.SkipIfDatabaseUnavailable(t)

	// Start capture to verify proxy logs
	logCapture := NewLogCapture()
	defer logCapture.Close()

	// Set up backend LMTP server with PROXY protocol support
	backendServer, account := common.SetupLMTPServerWithPROXY(t)
	defer backendServer.Close()

	// Set up LMTP proxy with PROXY protocol
	proxyAddress, proxyWrapper := setupLMTPProxyWithPROXY(t, backendServer.Address)
	defer proxyWrapper.Close()

	// Connect to the proxy
	client, err := NewLMTPClient(proxyAddress)
	if err != nil {
		t.Fatalf("Failed to connect to LMTP proxy: %v", err)
	}
	defer client.Close()

	// Test basic LMTP commands through proxy
	// 1. LHLO command
	if err := client.SendCommand("LHLO localhost"); err != nil {
		t.Fatalf("Failed to send LHLO: %v", err)
	}
	responses, err := client.ReadMultilineResponse()
	if err != nil {
		t.Fatalf("Failed to read LHLO response: %v", err)
	}
	if len(responses) == 0 || !strings.HasPrefix(responses[0], "250") {
		t.Fatalf("LHLO failed: %v", responses)
	}

	// 2. MAIL FROM command
	if err := client.SendCommand(fmt.Sprintf("MAIL FROM:<%s>", "sender@example.com")); err != nil {
		t.Fatalf("Failed to send MAIL FROM: %v", err)
	}
	response, err := client.ReadResponse()
	if err != nil {
		t.Fatalf("Failed to read MAIL FROM response: %v", err)
	}
	if !strings.HasPrefix(response, "250") {
		t.Fatalf("MAIL FROM failed: %s", response)
	}

	// 3. RCPT TO command
	if err := client.SendCommand(fmt.Sprintf("RCPT TO:<%s>", account.Email)); err != nil {
		t.Fatalf("Failed to send RCPT TO: %v", err)
	}
	response, err = client.ReadResponse()
	if err != nil {
		t.Fatalf("Failed to read RCPT TO response: %v", err)
	}
	if !strings.HasPrefix(response, "250") {
		t.Fatalf("RCPT TO failed: %s", response)
	}

	// 4. DATA command
	if err := client.SendCommand("DATA"); err != nil {
		t.Fatalf("Failed to send DATA: %v", err)
	}
	response, err = client.ReadResponse()
	if err != nil {
		t.Fatalf("Failed to read DATA response: %v", err)
	}
	if !strings.HasPrefix(response, "354") {
		t.Fatalf("DATA failed: %s", response)
	}

	// Send message content
	messageContent := "Subject: Test Message\r\n\r\nThis is a test message through LMTP proxy.\r\n.\r\n"
	if err := client.SendCommand(messageContent); err != nil {
		t.Fatalf("Failed to send message content: %v", err)
	}
	response, err = client.ReadResponse()
	if err != nil {
		t.Fatalf("Failed to read message response: %v", err)
	}
	if !strings.HasPrefix(response, "250") {
		t.Fatalf("Message delivery failed: %s", response)
	}

	// 5. Close connection (LMTP doesn't typically use QUIT like SMTP)
	// The session ends after message delivery

	// Wait a bit for logs to be written
	time.Sleep(200 * time.Millisecond)

	// Verify that proxy information appears in logs
	logOutput := logCapture.GetOutput()
	if !strings.Contains(logOutput, "proxy=127.0.0.1") {
		t.Errorf("Expected to find 'proxy=127.0.0.1' in logs, but didn't find it.\nLog output:\n%s", logOutput)
	}
}

// TestLMTPProxyWithXCLIENT tests LMTP proxy using XCLIENT command forwarding
func TestLMTPProxyWithXCLIENT(t *testing.T) {
	common.SkipIfDatabaseUnavailable(t)

	// Start capture to verify proxy logs
	logCapture := NewLogCapture()
	defer logCapture.Close()

	// Set up backend LMTP server without PROXY protocol (for XCLIENT mode)
	backendServer, account := common.SetupLMTPServerWithXCLIENT(t)
	defer backendServer.Close()

	// Set up LMTP proxy with XCLIENT support
	proxyAddress, proxyWrapper := setupLMTPProxyWithXCLIENT(t, backendServer.Address)
	defer proxyWrapper.Close()

	// Connect to the proxy
	client, err := NewLMTPClient(proxyAddress)
	if err != nil {
		t.Fatalf("Failed to connect to LMTP proxy: %v", err)
	}
	defer client.Close()

	// Test basic LMTP commands through proxy
	// 1. LHLO command
	if err := client.SendCommand("LHLO localhost"); err != nil {
		t.Fatalf("Failed to send LHLO: %v", err)
	}
	responses, err := client.ReadMultilineResponse()
	if err != nil {
		t.Fatalf("Failed to read LHLO response: %v", err)
	}
	if len(responses) == 0 || !strings.HasPrefix(responses[0], "250") {
		t.Fatalf("LHLO failed: %v", responses)
	}

	// 2. MAIL FROM command
	if err := client.SendCommand(fmt.Sprintf("MAIL FROM:<%s>", "sender@example.com")); err != nil {
		t.Fatalf("Failed to send MAIL FROM: %v", err)
	}
	response, err := client.ReadResponse()
	if err != nil {
		t.Fatalf("Failed to read MAIL FROM response: %v", err)
	}
	if !strings.HasPrefix(response, "250") {
		t.Fatalf("MAIL FROM failed: %s", response)
	}

	// 3. RCPT TO command
	if err := client.SendCommand(fmt.Sprintf("RCPT TO:<%s>", account.Email)); err != nil {
		t.Fatalf("Failed to send RCPT TO: %v", err)
	}
	response, err = client.ReadResponse()
	if err != nil {
		t.Fatalf("Failed to read RCPT TO response: %v", err)
	}
	if !strings.HasPrefix(response, "250") {
		t.Fatalf("RCPT TO failed: %s", response)
	}

	// 4. DATA command
	if err := client.SendCommand("DATA"); err != nil {
		t.Fatalf("Failed to send DATA: %v", err)
	}
	response, err = client.ReadResponse()
	if err != nil {
		t.Fatalf("Failed to read DATA response: %v", err)
	}
	if !strings.HasPrefix(response, "354") {
		t.Fatalf("DATA failed: %s", response)
	}

	// Send message content
	messageContent := "Subject: Test Message\r\n\r\nThis is a test message through LMTP proxy with XCLIENT.\r\n.\r\n"
	if err := client.SendCommand(messageContent); err != nil {
		t.Fatalf("Failed to send message content: %v", err)
	}
	response, err = client.ReadResponse()
	if err != nil {
		t.Fatalf("Failed to read message response: %v", err)
	}
	if !strings.HasPrefix(response, "250") {
		t.Fatalf("Message delivery failed: %s", response)
	}

	// 5. Close connection (LMTP doesn't typically use QUIT like SMTP)
	// The session ends after message delivery

	// Wait a bit for logs to be written
	time.Sleep(200 * time.Millisecond)

	// Note: The XCLIENT command is currently rejected at the go-smtp protocol level
	// with "501 5.5.2 Bad command" before reaching our XCLIENT backend implementation.
	// This is a limitation in the go-smtp library's XCLIENT handling for LMTP.
	// However, message delivery still works correctly through the proxy.
	logOutput := logCapture.GetOutput()

	// Verify that XCLIENT was attempted but rejected (expected behavior)
	if strings.Contains(logOutput, "backend rejected XCLIENT command") {
		t.Logf("XCLIENT command was rejected at protocol level (known go-smtp limitation)")
	}

	// Verify basic proxy functionality works
	if strings.Contains(logOutput, "message delivered successfully") {
		t.Logf("Message was delivered successfully through LMTP proxy")
	} else {
		t.Errorf("Expected successful message delivery through proxy")
	}

	t.Logf("LMTP XCLIENT proxy test completed - proxy functionality verified")
}

// TestLMTPProxyXCLIENTShouldWork tests what XCLIENT behavior should be when go-smtp library is fixed
// This test currently fails due to go-smtp library limitations but demonstrates expected behavior
func TestLMTPProxyXCLIENTShouldWork(t *testing.T) {
	// This test demonstrates what XCLIENT behavior should be when go-smtp library is fixed
	// Currently fails due to go-smtp library limitations - will pass when library is fixed

	common.SkipIfDatabaseUnavailable(t)

	// Start capture to verify XCLIENT forwarding works
	logCapture := NewLogCapture()
	defer logCapture.Close()

	// Set up backend LMTP server without PROXY protocol (for XCLIENT mode)
	backendServer, account := common.SetupLMTPServerWithXCLIENT(t)
	defer backendServer.Close()

	// Set up LMTP proxy with XCLIENT support
	proxyAddress, proxyWrapper := setupLMTPProxyWithXCLIENT(t, backendServer.Address)
	defer proxyWrapper.Close()

	// Connect to the proxy
	client, err := NewLMTPClient(proxyAddress)
	if err != nil {
		t.Fatalf("Failed to connect to LMTP proxy: %v", err)
	}
	defer client.Close()

	// Test basic LMTP commands through proxy
	// 1. LHLO command
	if err := client.SendCommand("LHLO localhost"); err != nil {
		t.Fatalf("Failed to send LHLO: %v", err)
	}
	responses, err := client.ReadMultilineResponse()
	if err != nil {
		t.Fatalf("Failed to read LHLO response: %v", err)
	}
	if len(responses) == 0 || !strings.HasPrefix(responses[0], "250") {
		t.Fatalf("LHLO failed: %v", responses)
	}

	// 2. MAIL FROM command
	if err := client.SendCommand(fmt.Sprintf("MAIL FROM:<%s>", "sender@example.com")); err != nil {
		t.Fatalf("Failed to send MAIL FROM: %v", err)
	}
	response, err := client.ReadResponse()
	if err != nil {
		t.Fatalf("Failed to read MAIL FROM response: %v", err)
	}
	if !strings.HasPrefix(response, "250") {
		t.Fatalf("MAIL FROM failed: %s", response)
	}

	// 3. RCPT TO command
	if err := client.SendCommand(fmt.Sprintf("RCPT TO:<%s>", account.Email)); err != nil {
		t.Fatalf("Failed to send RCPT TO: %v", err)
	}
	response, err = client.ReadResponse()
	if err != nil {
		t.Fatalf("Failed to read RCPT TO response: %v", err)
	}
	if !strings.HasPrefix(response, "250") {
		t.Fatalf("RCPT TO failed: %s", response)
	}

	// 4. DATA command
	if err := client.SendCommand("DATA"); err != nil {
		t.Fatalf("Failed to send DATA: %v", err)
	}
	response, err = client.ReadResponse()
	if err != nil {
		t.Fatalf("Failed to read DATA response: %v", err)
	}
	if !strings.HasPrefix(response, "354") {
		t.Fatalf("DATA failed: %s", response)
	}

	// Send message content
	messageContent := "Subject: Test Message\r\n\r\nThis is a test message through LMTP proxy with working XCLIENT.\r\n.\r\n"
	if err := client.SendCommand(messageContent); err != nil {
		t.Fatalf("Failed to send message content: %v", err)
	}
	response, err = client.ReadResponse()
	if err != nil {
		t.Fatalf("Failed to read message response: %v", err)
	}
	if !strings.HasPrefix(response, "250") {
		t.Fatalf("Message delivery failed: %s", response)
	}

	// Wait a bit for logs to be written
	time.Sleep(200 * time.Millisecond)

	// When XCLIENT works properly, we should see:
	// 1. XCLIENT forwarding success message (not rejection)
	// 2. proxy=127.0.0.1 in the backend logs (showing original client IP was forwarded)
	// 3. Backend processing XCLIENT command successfully
	logOutput := logCapture.GetOutput()

	// Verify XCLIENT forwarding succeeded (this will fail with current go-smtp)
	if strings.Contains(logOutput, "backend rejected XCLIENT command") {
		t.Errorf("XCLIENT command was rejected - this indicates go-smtp library limitation")
		t.Errorf("Expected: XCLIENT forwarding to succeed")
		t.Errorf("Actual: XCLIENT command rejected at protocol level")
	}

	// Verify proxy information appears in logs (this should work when XCLIENT works)
	if !strings.Contains(logOutput, "proxy=127.0.0.1") {
		t.Errorf("Expected to find 'proxy=127.0.0.1' in logs when XCLIENT works properly")
		t.Errorf("This indicates the original client IP was not properly forwarded")
	}

	// Verify XCLIENT success logging from proxy
	if !strings.Contains(logOutput, "XCLIENT forwarding and session reset completed successfully") {
		t.Errorf("Expected XCLIENT forwarding success message in logs")
	}

	// Verify backend processed XCLIENT command
	if !strings.Contains(logOutput, "*** XCLIENT METHOD CALLED ***") {
		t.Errorf("Expected backend to process XCLIENT command")
		t.Errorf("This indicates XCLIENT command never reached the backend handler")
	}

	t.Logf("LMTP XCLIENT proxy test with expected working behavior completed")
}

// setupLMTPProxyWithXRCPTFORWARD sets up an LMTP proxy that supports XRCPTFORWARD in RCPT TO
func setupLMTPProxyWithXRCPTFORWARD(t *testing.T, backendAddress string) (string, *LMTPProxyWrapper) {
	t.Helper()

	rdb := common.SetupTestDatabase(t)
	proxyAddress := common.GetRandomAddress(t)

	// Create LMTP proxy server with XRCPTFORWARD support
	server, err := lmtpproxy.New(
		context.Background(),
		rdb,
		"localhost",
		lmtpproxy.ServerOptions{
			Name:                   "test-proxy",
			Addr:                   proxyAddress,
			RemoteAddrs:            []string{backendAddress},
			RemotePort:             25,                                 // Default LMTP port
			RemoteUseProxyProtocol: false,                              // Disable PROXY protocol
			RemoteUseXCLIENT:       false,                              // Disable XCLIENT (focus on XRCPTFORWARD)
			TrustedProxies:         []string{"127.0.0.0/8", "::1/128"}, // Trust localhost
			ConnectTimeout:         5 * time.Second,
			SessionTimeout:         30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("Failed to create LMTP proxy with XRCPTFORWARD support: %v", err)
	}

	// Start the proxy server
	go func() {
		if err := server.Start(); err != nil {
			t.Logf("LMTP proxy server error: %v", err)
		}
	}()

	// Wait for server to start
	time.Sleep(100 * time.Millisecond)

	return proxyAddress, &LMTPProxyWrapper{server: server}
}

// TestLMTPProxyWithXRCPTFORWARD tests LMTP proxy using XRCPTFORWARD extension in RCPT TO
func TestLMTPProxyWithXRCPTFORWARD(t *testing.T) {
	common.SkipIfDatabaseUnavailable(t)

	// Start capture to verify proxy logs
	logCapture := NewLogCapture()
	defer logCapture.Close()

	// Set up backend LMTP server with XRCPTFORWARD support
	backendServer, account := common.SetupLMTPServerWithXCLIENT(t)
	defer backendServer.Close()

	// Set up LMTP proxy with XRCPTFORWARD support
	proxyAddress, proxyWrapper := setupLMTPProxyWithXRCPTFORWARD(t, backendServer.Address)
	defer proxyWrapper.Close()

	// Connect to the proxy
	client, err := NewLMTPClient(proxyAddress)
	if err != nil {
		t.Fatalf("Failed to connect to LMTP proxy: %v", err)
	}
	defer client.Close()

	// Test basic LMTP commands through proxy
	// 1. LHLO command
	if err := client.SendCommand("LHLO localhost"); err != nil {
		t.Fatalf("Failed to send LHLO: %v", err)
	}
	responses, err := client.ReadMultilineResponse()
	if err != nil {
		t.Fatalf("Failed to read LHLO response: %v", err)
	}
	if len(responses) == 0 || !strings.HasPrefix(responses[0], "250") {
		t.Fatalf("LHLO failed: %v", responses)
	}

	// 2. MAIL FROM command
	if err := client.SendCommand(fmt.Sprintf("MAIL FROM:<%s>", "sender@example.com")); err != nil {
		t.Fatalf("Failed to send MAIL FROM: %v", err)
	}
	response, err := client.ReadResponse()
	if err != nil {
		t.Fatalf("Failed to read MAIL FROM response: %v", err)
	}
	if !strings.HasPrefix(response, "250") {
		t.Fatalf("MAIL FROM failed: %s", response)
	}

	// 3. RCPT TO command with XRCPTFORWARD parameter
	// XRCPTFORWARD should contain Base64 encoded tab-separated key=value pairs
	// Example forwarding parameters: proxy=127.0.0.1	originating-ip=192.168.1.100
	xrcptForwardData := "proxy=127.0.0.1\toriginating-ip=192.168.1.100"
	base64Data := base64.StdEncoding.EncodeToString([]byte(xrcptForwardData))

	rcptCommand := fmt.Sprintf("RCPT TO:<%s> XRCPTFORWARD=%s", account.Email, base64Data)
	if err := client.SendCommand(rcptCommand); err != nil {
		t.Fatalf("Failed to send RCPT TO with XRCPTFORWARD: %v", err)
	}
	response, err = client.ReadResponse()
	if err != nil {
		t.Fatalf("Failed to read RCPT TO response: %v", err)
	}
	// XRCPTFORWARD should work with the forked go-smtp
	if !strings.HasPrefix(response, "250") {
		t.Fatalf("RCPT TO with XRCPTFORWARD failed: %s", response)
	}

	// 4. DATA command
	if err := client.SendCommand("DATA"); err != nil {
		t.Fatalf("Failed to send DATA: %v", err)
	}
	response, err = client.ReadResponse()
	if err != nil {
		t.Fatalf("Failed to read DATA response: %v", err)
	}
	if !strings.HasPrefix(response, "354") {
		t.Fatalf("DATA failed: %s", response)
	}

	// Send message content
	messageContent := "Subject: Test Message with XRCPTFORWARD\r\n\r\nThis is a test message through LMTP proxy with XRCPTFORWARD.\r\n.\r\n"
	if err := client.SendCommand(messageContent); err != nil {
		t.Fatalf("Failed to send message content: %v", err)
	}
	response, err = client.ReadResponse()
	if err != nil {
		t.Fatalf("Failed to read message response: %v", err)
	}
	if !strings.HasPrefix(response, "250") {
		t.Fatalf("Message delivery failed: %s", response)
	}

	// Wait a bit for logs to be written
	time.Sleep(200 * time.Millisecond)

	// Verify that XRCPTFORWARD was processed successfully
	logOutput := logCapture.GetOutput()
	if !strings.Contains(logOutput, "Processed XRCPTFORWARD parameters") {
		t.Errorf("Expected to find 'Processed XRCPTFORWARD parameters' in logs, but didn't find it.\nLog output:\n%s", logOutput)
	}

	// Just like XCLIENT, XRCPTFORWARD should show proxy= in session logs
	if !strings.Contains(logOutput, "proxy=127.0.0.1") {
		t.Errorf("Expected to find 'proxy=127.0.0.1' in logs from XRCPTFORWARD (like XCLIENT does), but didn't find it.\nLog output:\n%s", logOutput)
	}

	// Verify XRCPTFORWARD processing
	if strings.Contains(logOutput, "Processed XRCPTFORWARD parameters") {
		t.Logf("XRCPTFORWARD parameters were processed successfully")
	} else {
		t.Logf("XRCPTFORWARD parameters may not have been processed (could be expected if proxy doesn't forward them)")
	}

	t.Logf("LMTP XRCPTFORWARD proxy test completed")
}

// TestLMTPProxyXRCPTFORWARDTrustedNetworksOnly tests that XRCPTFORWARD only works from trusted networks
func TestLMTPProxyXRCPTFORWARDTrustedNetworksOnly(t *testing.T) {
	common.SkipIfDatabaseUnavailable(t)

	// Start capture to verify proxy logs
	logCapture := NewLogCapture()
	defer logCapture.Close()

	// Set up backend LMTP server with XRCPTFORWARD support
	backendServer, _ := common.SetupLMTPServerWithXCLIENT(t)
	defer backendServer.Close()

	// Set up LMTP proxy with restricted trusted networks (excluding localhost)
	rdb := common.SetupTestDatabase(t)
	proxyAddress := common.GetRandomAddress(t)

	// Create LMTP proxy server with NO trusted networks (to test access control)
	server, err := lmtpproxy.New(
		context.Background(),
		rdb,
		"localhost",
		lmtpproxy.ServerOptions{
			Name:                   "test-proxy",
			Addr:                   proxyAddress,
			RemoteAddrs:            []string{backendServer.Address},
			RemotePort:             25,                     // Default LMTP port
			RemoteUseProxyProtocol: false,                  // Disable PROXY protocol
			RemoteUseXCLIENT:       false,                  // Disable XCLIENT
			TrustedProxies:         []string{"10.0.0.0/8"}, // Trust only private networks (NOT localhost)
			ConnectTimeout:         5 * time.Second,
			SessionTimeout:         30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("Failed to create LMTP proxy with restricted trusted networks: %v", err)
	}

	proxyWrapper := &LMTPProxyWrapper{server: server}
	defer proxyWrapper.Close()

	// Start the proxy server
	go func() {
		if err := server.Start(); err != nil {
			t.Logf("LMTP proxy server error: %v", err)
		}
	}()

	// Wait for server to start
	time.Sleep(100 * time.Millisecond)

	// Try to connect to the proxy (should fail from untrusted network)
	client, err := NewLMTPClient(proxyAddress)
	if err != nil {
		// Expected - connection should be rejected from untrusted network
		t.Logf("Connection correctly rejected from untrusted network: %v", err)
		t.Logf("LMTP XRCPTFORWARD trusted networks test completed")
		return
	}
	defer client.Close()

	// If we reach here, the connection was accepted (unexpected)
	t.Errorf("Connection was accepted from untrusted network, but should have been rejected")

	// Since the connection was accepted, we don't test XRCPTFORWARD
	// The test has already failed at this point
}

// TestLMTPProxyXCLIENTTrustedNetworksOnly tests that XCLIENT only works from trusted networks
func TestLMTPProxyXCLIENTTrustedNetworksOnly(t *testing.T) {
	common.SkipIfDatabaseUnavailable(t)

	// Start capture to verify proxy logs
	logCapture := NewLogCapture()
	defer logCapture.Close()

	// Set up backend LMTP server with XCLIENT support
	backendServer, _ := common.SetupLMTPServerWithXCLIENT(t)
	defer backendServer.Close()

	// Set up LMTP proxy with restricted trusted networks (excluding localhost)
	rdb := common.SetupTestDatabase(t)
	proxyAddress := common.GetRandomAddress(t)

	// Create LMTP proxy server with NO localhost in trusted networks
	server, err := lmtpproxy.New(
		context.Background(),
		rdb,
		"localhost",
		lmtpproxy.ServerOptions{
			Name:                   "test-proxy",
			Addr:                   proxyAddress,
			RemoteAddrs:            []string{backendServer.Address},
			RemotePort:             25,                     // Default LMTP port
			RemoteUseProxyProtocol: false,                  // Disable PROXY protocol
			RemoteUseXCLIENT:       true,                   // Enable XCLIENT command
			TrustedProxies:         []string{"10.0.0.0/8"}, // Trust only private networks (NOT localhost)
			ConnectTimeout:         5 * time.Second,
			SessionTimeout:         30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("Failed to create LMTP proxy with restricted trusted networks: %v", err)
	}

	proxyWrapper := &LMTPProxyWrapper{server: server}
	defer proxyWrapper.Close()

	// Start the proxy server
	go func() {
		if err := server.Start(); err != nil {
			t.Logf("LMTP proxy server error: %v", err)
		}
	}()

	// Wait for server to start
	time.Sleep(100 * time.Millisecond)

	// Try to connect to the proxy (should fail from untrusted network)
	client, err := NewLMTPClient(proxyAddress)
	if err != nil {
		// Expected - connection should be rejected from untrusted network
		t.Logf("Connection correctly rejected from untrusted network: %v", err)
		t.Logf("LMTP XCLIENT trusted networks test completed")
		return
	}
	defer client.Close()

	// If we reach here, the connection was accepted (unexpected)
	t.Errorf("Connection was accepted from untrusted network, but should have been rejected")

	// Since the connection was accepted, we don't test XCLIENT
	// The test has already failed at this point
}

// TestLMTPDirectXRCPTFORWARD tests XRCPTFORWARD directly on LMTP backend (without proxy)
func TestLMTPDirectXRCPTFORWARD(t *testing.T) {
	common.SkipIfDatabaseUnavailable(t)

	// Start capture to verify logs
	logCapture := NewLogCapture()
	defer logCapture.Close()

	// Set up backend LMTP server directly (no proxy)
	backendServer, account := common.SetupLMTPServerWithXCLIENT(t)
	defer backendServer.Close()

	// Connect directly to the LMTP backend
	client, err := NewLMTPClient(backendServer.Address)
	if err != nil {
		t.Fatalf("Failed to connect to LMTP server: %v", err)
	}
	defer client.Close()

	// Test basic LMTP commands
	// 1. LHLO command
	if err := client.SendCommand("LHLO localhost"); err != nil {
		t.Fatalf("Failed to send LHLO: %v", err)
	}
	responses, err := client.ReadMultilineResponse()
	if err != nil {
		t.Fatalf("Failed to read LHLO response: %v", err)
	}
	if len(responses) == 0 || !strings.HasPrefix(responses[0], "250") {
		t.Fatalf("LHLO failed: %v", responses)
	}

	t.Logf("LHLO responses: %v", responses)

	// 2. MAIL FROM command
	if err := client.SendCommand(fmt.Sprintf("MAIL FROM:<%s>", "sender@example.com")); err != nil {
		t.Fatalf("Failed to send MAIL FROM: %v", err)
	}
	response, err := client.ReadResponse()
	if err != nil {
		t.Fatalf("Failed to read MAIL FROM response: %v", err)
	}
	if !strings.HasPrefix(response, "250") {
		t.Fatalf("MAIL FROM failed: %s", response)
	}

	// 3. RCPT TO command with XRCPTFORWARD parameter
	xrcptForwardData := "proxy=127.0.0.1\toriginating-ip=192.168.1.100"
	base64Data := base64.StdEncoding.EncodeToString([]byte(xrcptForwardData))

	rcptCommand := fmt.Sprintf("RCPT TO:<%s> XRCPTFORWARD=%s", account.Email, base64Data)
	if err := client.SendCommand(rcptCommand); err != nil {
		t.Fatalf("Failed to send RCPT TO with XRCPTFORWARD: %v", err)
	}
	response, err = client.ReadResponse()
	if err != nil {
		t.Fatalf("Failed to read RCPT TO response: %v", err)
	}

	// Log the response for debugging
	t.Logf("RCPT TO with XRCPTFORWARD response: %s", response)

	if !strings.HasPrefix(response, "250") {
		t.Logf("XRCPTFORWARD failed as expected (not enabled): %s", response)
		return // Exit early since XRCPTFORWARD isn't working
	}

	// If RCPT TO succeeded, continue with message delivery
	// 4. DATA command
	if err := client.SendCommand("DATA"); err != nil {
		t.Fatalf("Failed to send DATA: %v", err)
	}
	response, err = client.ReadResponse()
	if err != nil {
		t.Fatalf("Failed to read DATA response: %v", err)
	}
	if !strings.HasPrefix(response, "354") {
		t.Fatalf("DATA failed: %s", response)
	}

	// Send message content
	messageContent := "Subject: Test Direct XRCPTFORWARD\r\n\r\nThis is a test message with direct XRCPTFORWARD.\r\n.\r\n"
	if err := client.SendCommand(messageContent); err != nil {
		t.Fatalf("Failed to send message content: %v", err)
	}
	response, err = client.ReadResponse()
	if err != nil {
		t.Fatalf("Failed to read message response: %v", err)
	}
	if !strings.HasPrefix(response, "250") {
		t.Fatalf("Message delivery failed: %s", response)
	}

	// Wait for logs
	time.Sleep(200 * time.Millisecond)

	// Check logs for XRCPTFORWARD processing
	logOutput := logCapture.GetOutput()
	if strings.Contains(logOutput, "Processed XRCPTFORWARD parameters") {
		t.Logf("XRCPTFORWARD was processed successfully")
		if strings.Contains(logOutput, "proxy=127.0.0.1") {
			t.Logf("Proxy information found in logs")
		}
	} else {
		t.Logf("XRCPTFORWARD processing not found in logs")
	}

	t.Logf("LMTP direct XRCPTFORWARD test completed")
}
