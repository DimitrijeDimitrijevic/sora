package managesieve

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/foxcpp/go-sieve"
	"github.com/migadu/sora/consts"
	"github.com/migadu/sora/helpers"
	"github.com/migadu/sora/pkg/metrics"
	"github.com/migadu/sora/server"
)

type ManageSieveSession struct {
	server.Session
	mutex         sync.RWMutex
	mutexHelper   *server.MutexTimeoutHelper
	server        *ManageSieveServer
	conn          *net.Conn          // Connection to the client
	*server.User                     // User associated with the session
	authenticated bool               // Flag to indicate if the user has been authenticated
	ctx           context.Context    // Context for this session
	cancel        context.CancelFunc // Function to cancel the session's context

	reader      *bufio.Reader
	writer      *bufio.Writer
	isTLS       bool
	useMasterDB bool   // Pin session to master DB after a write to ensure consistency
	releaseConn func() // Function to release connection from limiter
	startTime   time.Time
}

func (s *ManageSieveSession) sendRawLine(line string) {
	s.writer.WriteString(line + "\r\n")
}

func (s *ManageSieveSession) sendCapabilities() {
	acquired, release := s.mutexHelper.AcquireReadLockWithTimeout()
	if !acquired {
		s.Log("WARNING: failed to acquire read lock for sendCapabilities")
		// Send minimal capabilities if lock fails
		s.sendRawLine(fmt.Sprintf("\"IMPLEMENTATION\" \"%s\"", "ManageSieve"))
		s.sendRawLine("\"SIEVE\" \"fileinto vacation\"")
		return
	}
	defer release()

	s.sendRawLine(fmt.Sprintf("\"IMPLEMENTATION\" \"%s\"", "ManageSieve"))

	// Build capabilities string from configured extensions
	extensionsStr := ""
	if len(s.server.supportedExtensions) > 0 {
		extensionsStr = strings.Join(s.server.supportedExtensions, " ")
	} else {
		extensionsStr = "fileinto vacation" // fallback
	}
	s.sendRawLine(fmt.Sprintf("\"SIEVE\" \"%s\"", extensionsStr))

	if s.server.tlsConfig != nil && s.server.useStartTLS && !s.isTLS {
		s.sendRawLine("\"STARTTLS\"")
	}
	if !s.isTLS && s.server.insecureAuth { // This check is safe under the read lock
		s.sendRawLine("\"AUTH=PLAIN\"")
	}
	if s.server.maxScriptSize > 0 {
		s.sendRawLine(fmt.Sprintf("\"MAXSCRIPTSIZE\" \"%d\"", s.server.maxScriptSize))
	}
}

func (s *ManageSieveSession) handleConnection() {
	defer s.Close()

	s.sendCapabilitiesGreeting()

	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				s.Log("client dropped connection")
			} else {
				s.Log("read error: %v", err)
			}
			return
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, " ", 3)
		command := strings.ToUpper(parts[0])

		// If debug logging is active, it might log the raw command.
		// This ensures that if any such logging exists, it will be of a masked line.
		// This is a defensive change as the direct logging is not visible in this file.
		s.Log(helpers.MaskSensitive(line, command, "AUTHENTICATE", "LOGIN"))

		switch command {
		case "CAPABILITY":
			start := time.Now()
			success := false
			defer func() {
				status := "failure"
				if success {
					status = "success"
				}
				metrics.CommandsTotal.WithLabelValues("managesieve", "CAPABILITY", status).Inc()
				metrics.CommandDuration.WithLabelValues("managesieve", "CAPABILITY").Observe(time.Since(start).Seconds())
			}()
			success = s.handleCapability()

		case "LOGIN":
			start := time.Now()
			success := false
			defer func() {
				status := "failure"
				if success {
					status = "success"
				}
				metrics.CommandsTotal.WithLabelValues("managesieve", "LOGIN", status).Inc()
				metrics.CommandDuration.WithLabelValues("managesieve", "LOGIN").Observe(time.Since(start).Seconds())
			}()

			if len(parts) < 3 {
				s.sendResponse("NO Syntax: LOGIN address password\r\n")
				continue
			}
			userAddress := parts[1]
			password := parts[2]

			address, err := server.NewAddress(userAddress)
			if err != nil {
				s.Log("error: %v", err)
				s.sendResponse("NO Invalid address\r\n")
				continue
			}

			// Get connection and proxy info for rate limiting
			netConn := *s.conn
			var proxyInfo *server.ProxyProtocolInfo
			if s.ProxyIP != "" {
				proxyInfo = &server.ProxyProtocolInfo{
					SrcIP: s.RemoteIP,
				}
			}

			// Apply progressive authentication delay BEFORE any other checks
			remoteAddr := &server.StringAddr{Addr: s.RemoteIP}
			server.ApplyAuthenticationDelay(s.ctx, s.server.authLimiter, remoteAddr, "MANAGESIEVE-LOGIN")

			// Check authentication rate limiting after delay
			if s.server.authLimiter != nil {
				if err := s.server.authLimiter.CanAttemptAuthWithProxy(s.ctx, netConn, proxyInfo, address.FullAddress()); err != nil {
					s.Log("[LOGIN] rate limited: %v", err)
					s.sendResponse("NO Too many authentication attempts. Please try again later.\r\n")
					continue
				}
			}

			// Try master password authentication first
			authSuccess := false
			var userID int64
			if len(s.server.masterSASLUsername) > 0 && len(s.server.masterSASLPassword) > 0 {
				if address.FullAddress() == string(s.server.masterSASLUsername) && password == string(s.server.masterSASLPassword) {
					s.Log("[LOGIN] Master password authentication successful for '%s'", address.FullAddress())
					authSuccess = true
					// For master password, we need to get the user ID
					userID, err = s.server.rdb.GetAccountIDByAddressWithRetry(s.ctx, address.FullAddress())
					if err != nil {
						s.Log("[LOGIN] Failed to get account ID for master user '%s': %v", address.FullAddress(), err)
						// Record failed attempt
						if s.server.authLimiter != nil {
							s.server.authLimiter.RecordAuthAttemptWithProxy(s.ctx, netConn, proxyInfo, address.FullAddress(), false)
						}
						metrics.AuthenticationAttempts.WithLabelValues("managesieve", "failure").Inc()
						s.sendResponse("NO Authentication failed\r\n")
						continue
					}
				}
			}

			// If master password didn't work, try regular authentication
			if !authSuccess {
				userID, err = s.server.rdb.AuthenticateWithRetry(s.ctx, address.FullAddress(), password)
				if err != nil {
					// Record failed attempt
					if s.server.authLimiter != nil {
						s.server.authLimiter.RecordAuthAttemptWithProxy(s.ctx, netConn, proxyInfo, address.FullAddress(), false)
					}
					metrics.AuthenticationAttempts.WithLabelValues("managesieve", "failure").Inc()
					s.sendResponse("NO Authentication failed\r\n")
					continue
				}
				authSuccess = true
			}

			// Record successful attempt
			if s.server.authLimiter != nil {
				s.server.authLimiter.RecordAuthAttemptWithProxy(s.ctx, netConn, proxyInfo, address.FullAddress(), true)
			}

			// Check if the context was cancelled during authentication logic
			if s.ctx.Err() != nil {
				s.Log("[LOGIN] context cancelled, aborting session update")
				continue
			}

			// Acquire write lock for updating session authentication state
			acquired, release := s.mutexHelper.AcquireWriteLockWithTimeout()
			if !acquired {
				s.Log("WARNING: failed to acquire write lock for Login command")
				s.sendResponse("NO Server busy, try again later\r\n")
				continue
			}
			defer release()

			s.authenticated = true
			s.User = server.NewUser(address, userID)

			// Increment authenticated connections counter
			authCount := s.server.authenticatedConnections.Add(1)
			totalCount := s.server.totalConnections.Load()
			s.Log("user %s authenticated (connections: total=%d, authenticated=%d)",
				address.FullAddress(), totalCount, authCount)

			// Track successful authentication
			metrics.AuthenticationAttempts.WithLabelValues("managesieve", "success").Inc()
			metrics.AuthenticatedConnectionsCurrent.WithLabelValues("managesieve").Inc()

			// Track domain and user connection activity
			if s.User != nil {
				metrics.TrackDomainConnection("managesieve", s.Domain())
				metrics.TrackUserActivity("managesieve", s.FullAddress(), "connection", 1)
			}

			s.sendResponse("OK Authenticated\r\n")
			success = true

		case "AUTHENTICATE":
			start := time.Now()
			success := false
			defer func() {
				status := "failure"
				if success {
					status = "success"
				}
				metrics.CommandsTotal.WithLabelValues("managesieve", "AUTHENTICATE", status).Inc()
				metrics.CommandDuration.WithLabelValues("managesieve", "AUTHENTICATE").Observe(time.Since(start).Seconds())
			}()
			success = s.handleAuthenticate(parts)

		case "LISTSCRIPTS":
			start := time.Now()
			success := false
			defer func() {
				status := "failure"
				if success {
					status = "success"
				}
				metrics.CommandsTotal.WithLabelValues("managesieve", "LISTSCRIPTS", status).Inc()
				metrics.CommandDuration.WithLabelValues("managesieve", "LISTSCRIPTS").Observe(time.Since(start).Seconds())
			}()

			if !s.authenticated {
				s.sendResponse("NO Not authenticated\r\n")
				continue
			}
			success = s.handleListScripts()

		case "GETSCRIPT":
			start := time.Now()
			success := false
			defer func() {
				status := "failure"
				if success {
					status = "success"
				}
				metrics.CommandsTotal.WithLabelValues("managesieve", "GETSCRIPT", status).Inc()
				metrics.CommandDuration.WithLabelValues("managesieve", "GETSCRIPT").Observe(time.Since(start).Seconds())
			}()

			if !s.authenticated {
				s.sendResponse("NO Not authenticated\r\n")
				continue
			}
			if len(parts) < 2 {
				s.sendResponse("NO Syntax: GETSCRIPT scriptName\r\n")
				continue
			}
			scriptName := parts[1]
			success = s.handleGetScript(scriptName)

		case "PUTSCRIPT":
			start := time.Now()
			success := false
			defer func() {
				status := "failure"
				if success {
					status = "success"
				}
				metrics.CommandsTotal.WithLabelValues("managesieve", "PUTSCRIPT", status).Inc()
				metrics.CommandDuration.WithLabelValues("managesieve", "PUTSCRIPT").Observe(time.Since(start).Seconds())
			}()

			if !s.authenticated {
				s.sendResponse("NO Not authenticated\r\n")
				continue
			}
			if len(parts) < 3 {
				s.sendResponse("NO Syntax: PUTSCRIPT scriptName scriptContent\r\n")
				continue
			}
			scriptName := parts[1]
			scriptContent := parts[2]

			// Check if script content is a literal string {length+}
			if strings.HasPrefix(scriptContent, "{") && strings.HasSuffix(scriptContent, "+}") {
				// Extract length from {length+}
				lengthStr := strings.TrimSuffix(strings.TrimPrefix(scriptContent, "{"), "+}")
				length := 0
				if _, err := fmt.Sscanf(lengthStr, "%d", &length); err != nil || length < 0 {
					s.sendResponse("NO Invalid literal string length\r\n")
					continue
				}

				// Send continuation response (+ ready for literal data)
				s.sendResponse("+\r\n")

				// Read the literal content (length bytes)
				literalContent := make([]byte, length)
				n, err := io.ReadFull(s.reader, literalContent)
				if err != nil || n != length {
					s.sendResponse("NO Failed to read literal string content\r\n")
					continue
				}
				scriptContent = string(literalContent)
			}

			success = s.handlePutScript(scriptName, scriptContent)

		case "SETACTIVE":
			start := time.Now()
			success := false
			defer func() {
				status := "failure"
				if success {
					status = "success"
				}
				metrics.CommandsTotal.WithLabelValues("managesieve", "SETACTIVE", status).Inc()
				metrics.CommandDuration.WithLabelValues("managesieve", "SETACTIVE").Observe(time.Since(start).Seconds())
			}()

			if !s.authenticated {
				s.sendResponse("NO Not authenticated\r\n")
				continue
			}
			if len(parts) < 2 {
				s.sendResponse("NO Syntax: SETACTIVE scriptName\r\n")
				continue
			}
			scriptName := parts[1]
			success = s.handleSetActive(scriptName)

		case "DELETESCRIPT":
			start := time.Now()
			success := false
			defer func() {
				status := "failure"
				if success {
					status = "success"
				}
				metrics.CommandsTotal.WithLabelValues("managesieve", "DELETESCRIPT", status).Inc()
				metrics.CommandDuration.WithLabelValues("managesieve", "DELETESCRIPT").Observe(time.Since(start).Seconds())
			}()

			if !s.authenticated {
				s.sendResponse("NO Not authenticated\r\n")
				continue
			}
			if len(parts) < 2 {
				s.sendResponse("NO Syntax: DELETESCRIPT scriptName\r\n")
				continue
			}
			scriptName := parts[1]
			success = s.handleDeleteScript(scriptName)

		case "STARTTLS":
			start := time.Now()
			success := false
			defer func() {
				status := "failure"
				if success {
					status = "success"
				}
				metrics.CommandsTotal.WithLabelValues("managesieve", "STARTTLS", status).Inc()
				metrics.CommandDuration.WithLabelValues("managesieve", "STARTTLS").Observe(time.Since(start).Seconds())
			}()

			if !s.server.useStartTLS || s.server.tlsConfig == nil {
				s.sendResponse("NO STARTTLS not supported\r\n")
				continue
			}
			if s.isTLS {
				s.sendResponse("NO TLS already active\r\n")
				continue
			}
			s.sendResponse("OK Begin TLS negotiation\r\n")

			// Upgrade the connection to TLS
			tlsConn := tls.Server(*s.conn, s.server.tlsConfig)
			if err := tlsConn.Handshake(); err != nil {
				s.Log("TLS handshake failed: %v", err)
				s.sendResponse("NO TLS handshake failed\r\n")
				continue
			}

			// Check if context was cancelled during handshake
			if s.ctx.Err() != nil {
				s.Log("[STARTTLS] context cancelled after handshake, aborting session update")
				return
			}

			// Acquire write lock for updating connection state
			acquired, release := s.mutexHelper.AcquireWriteLockWithTimeout()
			if !acquired {
				s.Log("failed to acquire write lock for STARTTLS command")
				s.sendResponse("NO Server busy, try again later\r\n")
				continue
			}
			defer release()

			// Replace the connection and readers/writers
			*s.conn = tlsConn
			s.reader = bufio.NewReader(tlsConn)
			s.writer = bufio.NewWriter(tlsConn)
			s.isTLS = true

			s.Log("TLS negotiation successful")
			success = true

		case "NOOP":
			start := time.Now()
			success := false
			defer func() {
				status := "failure"
				if success {
					status = "success"
				}
				metrics.CommandsTotal.WithLabelValues("managesieve", "NOOP", status).Inc()
				metrics.CommandDuration.WithLabelValues("managesieve", "NOOP").Observe(time.Since(start).Seconds())
			}()

			s.sendResponse("OK\r\n")
			success = true

		case "LOGOUT":
			start := time.Now()
			success := false
			defer func() {
				status := "failure"
				if success {
					status = "success"
				}
				metrics.CommandsTotal.WithLabelValues("managesieve", "LOGOUT", status).Inc()
				metrics.CommandDuration.WithLabelValues("managesieve", "LOGOUT").Observe(time.Since(start).Seconds())
			}()

			s.sendResponse("OK Goodbye\r\n")
			s.Close()
			success = true
			return

		default:
			s.sendResponse("NO Unknown command\r\n")
		}
	}
}

func (s *ManageSieveSession) sendCapabilitiesGreeting() {
	s.sendCapabilities()

	implementationName := "Sora"
	var okMessage string
	// Check if STARTTLS is supported and not yet active for the (STARTTLS) hint in OK response
	if s.server.tlsConfig != nil && s.server.useStartTLS && !s.isTLS {
		okMessage = fmt.Sprintf("OK (STARTTLS) \"%s\" ManageSieve server ready.", implementationName)
	} else {
		okMessage = fmt.Sprintf("OK \"%s\" ManageSieve server ready.", implementationName)
	}
	s.sendRawLine(okMessage)
	s.writer.Flush() // Flush all greeting lines
}

func (s *ManageSieveSession) sendResponse(response string) {
	s.writer.WriteString(response)
	s.writer.Flush()
}

func (s *ManageSieveSession) handleCapability() bool {
	s.sendCapabilities()
	s.sendRawLine("OK")
	s.writer.Flush()
	return true
}

func (s *ManageSieveSession) handleListScripts() bool {
	// Check if the context is closing before proceeding.
	if s.ctx.Err() != nil {
		s.Log("[LISTSCRIPTS] context cancelled, aborting command")
		s.sendResponse("NO Session closed\r\n")
		return false
	}

	// Acquire a read lock only to get the necessary session state.
	// A write lock is not needed for a read-only command.
	acquired, release := s.mutexHelper.AcquireReadLockWithTimeout()
	if !acquired {
		s.Log("WARNING: failed to acquire read lock for ListScripts command")
		s.sendResponse("NO Server busy, try again later\r\n")
		return false
	}
	// Copy the necessary state under lock.
	userID := s.UserID()
	useMaster := s.useMasterDB
	release() // Release lock before DB call

	// Create a context for read operations that respects session pinning
	readCtx := s.ctx
	if useMaster {
		readCtx = context.WithValue(s.ctx, consts.UseMasterDBKey, true)
	}

	scripts, err := s.server.rdb.GetUserScriptsWithRetry(readCtx, userID)
	if err != nil {
		s.sendResponse("NO Internal server error\r\n")
		return false
	}

	if len(scripts) == 0 {
		s.sendResponse("OK\r\n")
		return true
	}

	for _, script := range scripts {
		line := fmt.Sprintf("\"%s\"", script.Name)
		if script.Active {
			line += " ACTIVE"
		}
		s.sendRawLine(line)
	}
	s.sendRawLine("OK")
	s.writer.Flush()
	return true
}

func (s *ManageSieveSession) handleGetScript(name string) bool {
	// Check if the context is closing before proceeding.
	if s.ctx.Err() != nil {
		s.Log("[GETSCRIPT] context cancelled, aborting command")
		s.sendResponse("NO Session closed\r\n")
		return false
	}

	// Acquire a read lock only to get the necessary session state.
	acquired, release := s.mutexHelper.AcquireReadLockWithTimeout()
	if !acquired {
		s.Log("WARNING: failed to acquire read lock for GetScript command")
		s.sendResponse("NO Server busy, try again later\r\n")
		return false
	}
	// Copy the necessary state under lock.
	userID := s.UserID()
	useMaster := s.useMasterDB
	release() // Release lock before DB call

	// Create a context for read operations that respects session pinning
	readCtx := s.ctx
	if useMaster {
		readCtx = context.WithValue(s.ctx, consts.UseMasterDBKey, true)
	}

	script, err := s.server.rdb.GetScriptByNameWithRetry(readCtx, name, userID)
	if err != nil {
		s.sendResponse("NO No such script\r\n")
		return false
	}
	s.writer.WriteString(fmt.Sprintf("{%d}\r\n", len(script.Script)))
	s.writer.WriteString(script.Script)
	s.writer.Flush()
	s.sendResponse("OK\r\n")
	return true
}

func (s *ManageSieveSession) handlePutScript(name, content string) bool {
	start := time.Now()
	// Check if the context is closing before proceeding.
	if s.ctx.Err() != nil {
		s.Log("[PUTSCRIPT] context cancelled, aborting command")
		s.sendResponse("NO Session closed\r\n")
		return false
	}

	// Validate script name - must not be empty and should be a valid identifier
	name = strings.Trim(name, "\"") // Remove surrounding quotes if present
	if name == "" {
		s.sendResponse("NO Script name cannot be empty\r\n")
		return false
	}

	// Phase 1: Read session state
	acquired, release := s.mutexHelper.AcquireReadLockWithTimeout()
	if !acquired {
		s.Log("WARNING: failed to acquire read lock for PutScript command")
		s.sendResponse("NO Server busy, try again later\r\n")
		return false
	}
	userID := s.UserID()
	useMaster := s.useMasterDB
	release()

	// Phase 2: Validate and perform DB operations
	if s.server.maxScriptSize > 0 && int64(len(content)) > s.server.maxScriptSize {
		s.sendResponse(fmt.Sprintf("NO (MAXSCRIPTSIZE) Script size %d exceeds maximum allowed size %d\r\n", len(content), s.server.maxScriptSize))
		return false
	}

	scriptReader := strings.NewReader(content)
	options := sieve.DefaultOptions()
	// Configure extensions based on server configuration
	// If no extensions are configured, none are supported
	options.EnabledExtensions = s.server.supportedExtensions
	_, err := sieve.Load(scriptReader, options)
	if err != nil {
		s.sendResponse(fmt.Sprintf("NO Script validation failed: %v\r\n", err))
		return false
	}

	// Create a context for read operations that respects session pinning
	readCtx := s.ctx
	if useMaster {
		readCtx = context.WithValue(s.ctx, consts.UseMasterDBKey, true)
	}

	script, err := s.server.rdb.GetScriptByNameWithRetry(readCtx, name, userID)
	if err != nil {
		if err != consts.ErrDBNotFound {
			s.sendResponse("NO Internal server error\r\n")
			return false
		}
	}

	var responseMsg string
	if script != nil {
		_, err := s.server.rdb.UpdateScriptWithRetry(s.ctx, script.ID, userID, name, content)
		if err != nil {
			s.sendResponse("NO Internal server error\r\n")
			return false
		}

		// Track script upload/update
		metrics.ManageSieveScriptsUploaded.Inc()
		metrics.CriticalOperationDuration.WithLabelValues("managesieve_putscript").Observe(time.Since(start).Seconds())

		// Track domain and user activity - PUTSCRIPT is script processing intensive!
		if s.User != nil {
			metrics.TrackDomainCommand("managesieve", s.Domain(), "PUTSCRIPT")
			metrics.TrackUserActivity("managesieve", s.FullAddress(), "command", 1)
		}

		responseMsg = "OK Script updated\r\n"
	} else {
		_, err = s.server.rdb.CreateScriptWithRetry(s.ctx, userID, name, content)
		if err != nil {
			s.sendResponse("NO Internal server error\r\n")
			return false
		}
		responseMsg = "OK Script stored\r\n"
	}

	// Phase 3: Update session state
	acquired, release = s.mutexHelper.AcquireWriteLockWithTimeout()
	if !acquired {
		s.Log("WARNING: failed to acquire write lock for PutScript command to pin session")
	} else {
		s.useMasterDB = true
		release()
	}

	// Track script upload/create
	metrics.ManageSieveScriptsUploaded.Inc()
	metrics.CriticalOperationDuration.WithLabelValues("managesieve_putscript").Observe(time.Since(start).Seconds())

	// Track domain and user activity - PUTSCRIPT is script processing intensive!
	if s.User != nil {
		metrics.TrackDomainCommand("managesieve", s.Domain(), "PUTSCRIPT")
		metrics.TrackUserActivity("managesieve", s.FullAddress(), "command", 1)
	}
	s.sendResponse(responseMsg)
	return true
}

func (s *ManageSieveSession) handleSetActive(name string) bool {
	start := time.Now()
	// Check if the context is closing before proceeding.
	if s.ctx.Err() != nil {
		s.Log("[SETACTIVE] context cancelled, aborting command")
		s.sendResponse("NO Session closed\r\n")
		return false
	}

	// Phase 1: Read session state
	acquired, release := s.mutexHelper.AcquireReadLockWithTimeout()
	if !acquired {
		s.Log("WARNING: failed to acquire read lock for SetActive command")
		s.sendResponse("NO Server busy, try again later\r\n")
		return false
	}
	userID := s.UserID()
	useMaster := s.useMasterDB
	release()

	// Phase 2: DB operations
	readCtx := s.ctx
	if useMaster {
		readCtx = context.WithValue(s.ctx, consts.UseMasterDBKey, true)
	}

	script, err := s.server.rdb.GetScriptByNameWithRetry(readCtx, name, userID)
	if err != nil {
		if err == consts.ErrDBNotFound {
			s.sendResponse("NO No such script\r\n")
			return false
		}
		s.sendResponse("NO Internal server error\r\n")
		return false
	}

	// Validate the script before activating it
	scriptReader := strings.NewReader(script.Script)
	options := sieve.DefaultOptions()
	// Configure extensions based on server configuration
	// If no extensions are configured, none are supported
	options.EnabledExtensions = s.server.supportedExtensions
	_, err = sieve.Load(scriptReader, options)
	if err != nil {
		s.sendResponse(fmt.Sprintf("NO Script validation failed: %v\r\n", err))
		return false
	}

	err = s.server.rdb.SetScriptActiveWithRetry(s.ctx, script.ID, userID, true)
	if err != nil {
		s.sendResponse("NO Internal server error\r\n")
		return false
	}

	// Phase 3: Update session state
	acquired, release = s.mutexHelper.AcquireWriteLockWithTimeout()
	if !acquired {
		s.Log("WARNING: failed to acquire write lock for SetActive command to pin session")
	} else {
		s.useMasterDB = true
		release()
	}

	// Track script activation
	metrics.ManageSieveScriptsActivated.Inc()
	metrics.CriticalOperationDuration.WithLabelValues("managesieve_setactive").Observe(time.Since(start).Seconds())

	s.sendResponse("OK Script activated\r\n")
	return true
}

func (s *ManageSieveSession) handleDeleteScript(name string) bool {
	// Check if the context is closing before proceeding.
	if s.ctx.Err() != nil {
		s.Log("[DELETESCRIPT] context cancelled, aborting command")
		s.sendResponse("NO Session closed\r\n")
		return false
	}

	// Phase 1: Read session state
	acquired, release := s.mutexHelper.AcquireReadLockWithTimeout()
	if !acquired {
		s.Log("WARNING: failed to acquire read lock for DeleteScript command")
		s.sendResponse("NO Server busy, try again later\r\n")
		return false
	}
	userID := s.UserID()
	useMaster := s.useMasterDB
	release()

	// Phase 2: DB operations
	readCtx := s.ctx
	if useMaster {
		readCtx = context.WithValue(s.ctx, consts.UseMasterDBKey, true)
	}

	script, err := s.server.rdb.GetScriptByNameWithRetry(readCtx, name, userID)
	if err != nil {
		if err == consts.ErrDBNotFound {
			s.sendResponse("NO No such script\r\n") // RFC uses NO for "No such script"
			return false
		}
		s.sendResponse("NO Internal server error\r\n") // RFC uses NO for server errors
		return false
	}

	err = s.server.rdb.DeleteScriptWithRetry(s.ctx, script.ID, userID)
	if err != nil {
		s.sendResponse("NO Internal server error\r\n")
		return false
	}

	// Phase 3: Update session state
	acquired, release = s.mutexHelper.AcquireWriteLockWithTimeout()
	if !acquired {
		s.Log("WARNING: failed to acquire write lock for DeleteScript command to pin session")
	} else {
		s.useMasterDB = true
		release()
	}
	s.sendResponse("OK Script deleted\r\n")
	return true
}

func (s *ManageSieveSession) Close() error {
	// Acquire write lock for cleanup
	acquired, release := s.mutexHelper.AcquireWriteLockWithTimeout()
	if !acquired {
		s.Log("WARNING: failed to acquire write lock for Close operation")
		// Continue with close even if we can't get the lock
	} else {
		defer release()
	}

	// Observe connection duration
	metrics.ConnectionDuration.WithLabelValues("managesieve").Observe(time.Since(s.startTime).Seconds())

	// Decrement connection counters
	totalCount := s.server.totalConnections.Add(-1)
	var authCount int64 = 0

	(*s.conn).Close()

	// Release connection from limiter
	if s.releaseConn != nil {
		s.releaseConn()
		s.releaseConn = nil // Prevent double release
	}

	// Prometheus metrics - connection closed
	metrics.ConnectionsCurrent.WithLabelValues("managesieve").Dec()

	if s.User != nil {
		// If authenticated, decrement the authenticated connections counter
		if s.authenticated {
			metrics.AuthenticatedConnectionsCurrent.WithLabelValues("managesieve").Dec()
			authCount = s.server.authenticatedConnections.Add(-1)
		} else {
			authCount = s.server.authenticatedConnections.Load()
		}
		s.Log("session closed (connections: total=%d, authenticated=%d)",
			totalCount, authCount)
		s.User = nil
		s.Id = ""
		s.authenticated = false
		if s.cancel != nil {
			s.cancel()
		}
	} else {
		authCount = s.server.authenticatedConnections.Load()
		s.Log("session closed unauthenticated (connections: total=%d, authenticated=%d)",
			totalCount, authCount)
	}
	return nil
}

func (s *ManageSieveSession) handleAuthenticate(parts []string) bool {
	start := time.Now()
	success := false
	defer func() {
		if !success {
			// Track failed authentication if not already successful
			metrics.AuthenticationAttempts.WithLabelValues("managesieve", "failure").Inc()
			metrics.CriticalOperationDuration.WithLabelValues("managesieve_authentication").Observe(time.Since(start).Seconds())
		}
	}()

	if len(parts) < 2 {
		s.sendResponse("NO Syntax: AUTHENTICATE mechanism\r\n")
		return false
	}

	mechanism := strings.ToUpper(parts[1])
	if mechanism != "PLAIN" {
		s.sendResponse("NO Unsupported authentication mechanism\r\n")
		return false
	}

	// Check if initial response is provided
	var authData string
	if len(parts) > 2 {
		// Initial response provided - need to decode from base64
		authData = parts[2]
	} else {
		// No initial response, send continuation
		s.sendResponse("\"\"\r\n")

		// Read the authentication data
		authLine, err := s.reader.ReadString('\n')
		if err != nil {
			s.Log("error reading auth data: %v", err)
			s.sendResponse("NO Authentication failed\r\n")
			return false
		}
		authData = strings.TrimSpace(authLine)

		// Check for cancellation
		if authData == "*" {
			s.sendResponse("NO Authentication cancelled\r\n")
			return false
		}
	}

	// Decode base64
	decoded, err := base64.StdEncoding.DecodeString(authData)
	if err != nil {
		s.Log("error decoding auth data: %v", err)
		s.sendResponse("NO Invalid authentication data\r\n")
		return false
	}

	// Parse SASL PLAIN format: [authz-id] \0 authn-id \0 password
	parts = strings.Split(string(decoded), "\x00")
	if len(parts) != 3 {
		s.Log("invalid SASL PLAIN format")
		s.sendResponse("NO Invalid authentication format\r\n")
		return false
	}

	authzID := parts[0]  // Authorization identity (who to act as)
	authnID := parts[1]  // Authentication identity (who is authenticating)
	password := parts[2] // Password

	s.Log("[SASL PLAIN] AuthorizationID: '%s', AuthenticationID: '%s'", authzID, authnID)

	// Check for Master SASL Authentication
	var userID int64
	var impersonating bool
	var targetAddress *server.Address

	if len(s.server.masterSASLUsername) > 0 && len(s.server.masterSASLPassword) > 0 {
		// Check if this is a master SASL login
		if authnID == string(s.server.masterSASLUsername) && password == string(s.server.masterSASLPassword) {
			// Master SASL authentication successful
			if authzID == "" {
				s.Log("[AUTH] Master SASL authentication for '%s' successful, but no authorization identity provided.", authnID)
				s.sendResponse("NO Master SASL login requires an authorization identity.\r\n")
				return false
			}

			s.Log("[AUTH] Master SASL user '%s' authenticated. Attempting to impersonate '%s'.", authnID, authzID)

			// Log in as the authzID without a password check
			address, err := server.NewAddress(authzID)
			if err != nil {
				s.Log("[AUTH] Failed to parse impersonation target user '%s': %v", authzID, err)
				s.sendResponse("NO Invalid impersonation target user format\r\n")
				return false
			}

			userID, err = s.server.rdb.GetAccountIDByAddressWithRetry(s.ctx, address.FullAddress())
			if err != nil {
				s.Log("[AUTH] Failed to get account ID for impersonation target user '%s': %v", authzID, err)
				s.sendResponse("NO Impersonation target user not found\r\n")
				return false
			}

			targetAddress = &address
			impersonating = true
		}
	}

	// If not using master SASL, perform regular authentication
	if !impersonating {
		// For regular ManageSieve, we don't support proxy authentication
		if authzID != "" && authzID != authnID {
			s.Log("proxy authentication not supported: authz='%s', authn='%s'", authzID, authnID)
			s.sendResponse("NO Proxy authentication not supported\r\n")
			return false
		}

		// Authenticate the user
		address, err := server.NewAddress(authnID)
		if err != nil {
			s.Log("invalid address format: %v", err)
			s.sendResponse("NO Invalid username format\r\n")
			return false
		}

		s.Log("authentication attempt for %s", address.FullAddress())

		// Get connection and proxy info for rate limiting
		netConn := *s.conn
		var proxyInfo *server.ProxyProtocolInfo
		if s.ProxyIP != "" {
			proxyInfo = &server.ProxyProtocolInfo{
				SrcIP: s.RemoteIP,
			}
		}

		// Apply progressive authentication delay BEFORE any other checks
		remoteAddr := &server.StringAddr{Addr: s.RemoteIP}
		server.ApplyAuthenticationDelay(s.ctx, s.server.authLimiter, remoteAddr, "MANAGESIEVE-SASL")

		// Check authentication rate limiting after delay
		if s.server.authLimiter != nil {
			if err := s.server.authLimiter.CanAttemptAuthWithProxy(s.ctx, netConn, proxyInfo, address.FullAddress()); err != nil {
				s.Log("[SASL PLAIN] rate limited: %v", err)
				s.sendResponse("NO Too many authentication attempts. Please try again later.\r\n")
				return false
			}
		}

		userID, err = s.server.rdb.AuthenticateWithRetry(s.ctx, address.FullAddress(), password)
		if err != nil {
			// Record failed attempt
			if s.server.authLimiter != nil {
				s.server.authLimiter.RecordAuthAttemptWithProxy(s.ctx, netConn, proxyInfo, address.FullAddress(), false)
			}
			s.sendResponse("NO Authentication failed\r\n")
			s.Log("authentication failed")
			return false
		}

		// Record successful attempt
		if s.server.authLimiter != nil {
			s.server.authLimiter.RecordAuthAttemptWithProxy(s.ctx, netConn, proxyInfo, address.FullAddress(), true)
		}

		targetAddress = &address
	}

	// Check if the context was cancelled during authentication logic
	if s.ctx.Err() != nil {
		s.Log("[AUTH] context cancelled, aborting session update")
		return false
	}

	// Acquire write lock for updating session authentication state
	acquired, release := s.mutexHelper.AcquireWriteLockWithTimeout()
	if !acquired {
		s.Log("WARNING: failed to acquire write lock for Authenticate command")
		s.sendResponse("NO Server busy, try again later\r\n")
		return false
	}
	defer release()

	s.authenticated = true
	s.User = server.NewUser(*targetAddress, userID)

	// Increment authenticated connections counter
	authCount := s.server.authenticatedConnections.Add(1)
	totalCount := s.server.totalConnections.Load()
	if impersonating {
		s.Log("authenticated via Master SASL PLAIN as '%s' (connections: total=%d, authenticated=%d)",
			targetAddress.FullAddress(), totalCount, authCount)
	} else {
		s.Log("authenticated via SASL PLAIN (connections: total=%d, authenticated=%d)",
			totalCount, authCount)
	}

	// Track successful authentication
	metrics.AuthenticationAttempts.WithLabelValues("managesieve", "success").Inc()
	metrics.AuthenticatedConnectionsCurrent.WithLabelValues("managesieve").Inc()
	metrics.CriticalOperationDuration.WithLabelValues("managesieve_authentication").Observe(time.Since(start).Seconds())

	// Track domain and user connection activity
	if s.User != nil {
		metrics.TrackDomainConnection("managesieve", s.Domain())
		metrics.TrackUserActivity("managesieve", s.FullAddress(), "connection", 1)
	}

	s.sendResponse("OK Authenticated\r\n")
	success = true
	return true
}
