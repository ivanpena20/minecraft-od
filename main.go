package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type MessagesConfig struct {
	MotdOffline  string   `json:"motd_offline"`
	OfflineHover []string `json:"offline_hover"`
	BootingUp    string   `json:"booting_up"`
	WorldLoading string   `json:"world_loading"`
}

type Config struct {
	ManagerPort string         `json:"manager_port"`
	BackendAddr string         `json:"backend_addr"`
	JarPath     string         `json:"jar_path"`
	JarFile     string         `json:"jar_file"`
	JavaArgs    string         `json:"java_args"`
	IdleMinutes int            `json:"idle_minutes"`
	Messages    MessagesConfig `json:"messages"`
}

var (
	isServerRunning bool
	serverMutex     sync.Mutex
	serverStdin     io.WriteCloser

	activePlayers int
	activeMutex   sync.Mutex
	shutdownTimer *time.Timer

	// Config and flags
	cfg        Config
	configPath = flag.String("config", "config.json", "Path to the configuration file")
	verbose    = flag.Bool("verbose", false, "Enable verbose logging")
	jarPathArg = flag.String("jarpath", "", "Path to the server directory (overrides config)")
	jarFileArg = flag.String("jarfile", "", "Server executable jar file (overrides config)")
	portArg    = flag.String("port", "", "Manager port to listen on (overrides config)")
)

// logInfo: prints standard information
func logInfo(format string, a ...interface{}) {
	fmt.Printf(format+"\n", a...)
}

// logVerbose: prints only if verbose flag is set
func logVerbose(format string, a ...interface{}) {
	if *verbose {
		fmt.Printf("[DEBUG] "+format+"\n", a...)
	}
}

// loadConfig: reads config.json or creates a default one
func loadConfig() {
	defaultConfig := Config{
		ManagerPort: "0.0.0.0:25565",
		BackendAddr: "127.0.0.1:25566",
		JarPath:     "/home/ivan/server",
		JarFile:     "server.jar",
		JavaArgs:    "-Xms2G -Xmx2G",
		IdleMinutes: 20,
		Messages: MessagesConfig{
			MotdOffline:  "§e[minecraft-od] §7Status: §cSleeping\n§7Join the server to start it up",
			OfflineHover: []string{
				"§cServer is sleeping",
				"§aJoin to wake it up!",
			},
			BootingUp:    "§e[Minecraft on Demand] §fThe server is booting up.\n§7Please wait a moment and try again.",
			WorldLoading: "§e[Minecraft on Demand] §fThe world is still loading.\n§7Give it just a few more seconds.",
		},
	}

	data, err := os.ReadFile(*configPath)
	if err != nil {
		if os.IsNotExist(err) {
			logInfo("[SYSTEM] Config file %s not found. Creating default...", *configPath)
			data, _ = json.MarshalIndent(defaultConfig, "", "  ")
			os.WriteFile(*configPath, data, 0644)
			cfg = defaultConfig
		} else {
			logInfo("[SYSTEM ERROR] Failed to read config: %v", err)
			os.Exit(1)
		}
	} else {
		err = json.Unmarshal(data, &cfg)
		if err != nil {
			logInfo("[SYSTEM ERROR] Failed to parse config JSON: %v", err)
			os.Exit(1)
		}
	}

	// Override with command line arguments
	if *jarPathArg != "" {
		cfg.JarPath = *jarPathArg
	}
	if *jarFileArg != "" {
		cfg.JarFile = *jarFileArg
	}
	if *portArg != "" {
		cfg.ManagerPort = *portArg
	}
}

func escapeJSONString(s string) string {
	// Escape newlines for valid JSON string payload
	return strings.ReplaceAll(s, "\n", "\\n")
}

func resizeTo64x64(src image.Image) image.Image {
	bounds := src.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()

	if width == 64 && height == 64 {
		return src
	}

	dst := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			srcX := x * width / 64
			srcY := y * height / 64
			dst.Set(x, y, src.At(bounds.Min.X+srcX, bounds.Min.Y+srcY))
		}
	}
	return dst
}

func getOfflineFavicon() string {
	iconPath := filepath.Join(cfg.JarPath, "server-icon.png")
	file, err := os.Open(iconPath)
	if err != nil {
		return ""
	}
	defer file.Close()

	img, _, err := image.Decode(file)
	if err != nil {
		logVerbose("Failed to decode server-icon.png: %v", err)
		return ""
	}

	resized := resizeTo64x64(img)

	var buf bytes.Buffer
	err = png.Encode(&buf, resized)
	if err != nil {
		logVerbose("Failed to encode resized server icon: %v", err)
		return ""
	}

	b64 := base64.StdEncoding.EncodeToString(buf.Bytes())
	return fmt.Sprintf(`,"favicon": "data:image/png;base64,%s"`, b64)
}

func generateHoverJSON() string {
	var hovers []string
	for i, line := range cfg.Messages.OfflineHover {
		id := fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
		hovers = append(hovers, fmt.Sprintf(`{"name": "%s", "id": "%s"}`, escapeJSONString(line), id))
	}
	return strings.Join(hovers, ",")
}

func main() {
	flag.Parse()
	loadConfig()

	listener, err := net.Listen("tcp", cfg.ManagerPort)
	if err != nil {
		logInfo("Failed to bind port %s: %v", cfg.ManagerPort, err)
		os.Exit(1)
	}
	defer listener.Close()

	logInfo("minecraft-od v0.2 by ivanpena")
	logInfo("listening on %s...", cfg.ManagerPort)

	// shutdown protection
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		logInfo("\n[SYSTEM] Closing safely...")
		
		serverMutex.Lock()
		if isServerRunning && serverStdin != nil {
			logInfo("[SYSTEM] Stopping server gracefully before exit...")
			serverStdin.Write([]byte("stop\n"))
			serverMutex.Unlock()
			
			time.Sleep(15 * time.Second)	// 15 sec for chunks to save
		} else {
			serverMutex.Unlock()
		}
		os.Exit(0)
	}()

	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			input := scanner.Text() + "\n"

			serverMutex.Lock()
			if isServerRunning && serverStdin != nil {
				// Pass commands directly to the active server
				serverStdin.Write([]byte(input))
			} else {
				// Warn if someone tries to run a command while the server sleeps
				logInfo("[SYSTEM] Command ignored: The server is offline.")
			}
			serverMutex.Unlock()
		}
		if err := scanner.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "stdin scanner error: %v\n", err)
		}
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go handleConnection(conn)
	}
}

// handleConnection: routes the traffic based on the Handshake
func handleConnection(conn net.Conn) {
	defer conn.Close() // close

	// 3-second read deadline prevents scanner bots from hanging this thread indefinitely.
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	// read packet length
	_, err := ReadVarInt(conn)
	if err != nil {
		return
	}

	// read packet id
	packetID, err := ReadVarInt(conn)
	if err != nil {
		return
	}
	// Minecraft 1.6 or older sends a Legacy Ping (0xFE). We ignore it.
	if packetID == 0xFE {
		return
	}

	// 0x00 = handshake id
	if packetID == 0x00 {
		protocolVersion, _ := ReadVarInt(conn)
		serverAddress, _ := ReadString(conn)
		portBuf := make([]byte, 2)
		io.ReadFull(conn, portBuf)

		nextState, _ := ReadVarInt(conn)

		// Lift the read deadline now that we know the player's intent, otherwise they'd disconnect mid-game
		conn.SetReadDeadline(time.Time{})

		// Proxy if the server is running, for both ping (1) and join (2)
		if isServerRunning {
			handleProxy(conn, protocolVersion, serverAddress, portBuf, nextState)
			return
		}

		switch nextState {
		case 1:
			// menu refresh/ping (server is offline)
			handleStatus(conn)
		case 2:
			// join server (server is offline)
			handleLoginKick(conn, protocolVersion)
		}
	}
}

// handleStatus: handles the offline status request and response
func handleStatus(conn net.Conn) {
	// Enforce a deadline so ping requests don't block forever if the client drops
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// The client sends a Request packet (Length 1, ID 0x00) immediately after Handshake
	_, _ = ReadVarInt(conn)
	_, _ = ReadVarInt(conn)

	faviconJSON := getOfflineFavicon()
	hoverJSON := generateHoverJSON()

	// Server is asleep, hide player max to indicate offline status
	jsonResponse := fmt.Sprintf(`{
		"version": { "name": "§cOffline", "protocol": -1 }, 
		"players": { "max": 0, "online": 0,
			"sample": [%s]
		},
		"description": { "text": "%s" }
		%s
	}`, hoverJSON, escapeJSONString(cfg.Messages.MotdOffline), faviconJSON)

	WriteStatusResponse(conn, jsonResponse)

	// read length id and payload from clients ping packet
	_, err := ReadVarInt(conn)
	if err != nil {
		return
	}
	pingID, err := ReadVarInt(conn)
	if err != nil || pingID != 0x01 {
		return
	}

	pingPayload := make([]byte, 8)
	io.ReadFull(conn, pingPayload)

	WritePongPacket(conn, pingPayload)

	// tcp half close
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.CloseWrite()
	}
	time.Sleep(50 * time.Millisecond)
}

// handleLoginKick: kicks the player and starts the server
func handleLoginKick(conn net.Conn, protocolVersion int) {
	logInfo("--> Player from %s attempted to join! (Protocol: %d)", conn.RemoteAddr().String(), protocolVersion)

	// kick player and show message
	jsonMsg := fmt.Sprintf(`{"text": "%s"}`, escapeJSONString(cfg.Messages.BootingUp))
	WriteDisconnectPacket(conn, jsonMsg)

	// tcp half close
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.CloseWrite()
	}
	time.Sleep(100 * time.Millisecond)

	serverMutex.Lock()
	if !isServerRunning {
		isServerRunning = true
		go startServer()
	}
	serverMutex.Unlock()
}

func handleProxy(clientConn net.Conn, protocolVersion int, serverAddress string, portBuf []byte, nextState int) {
	// rebuild initial handshake

	backendConn, err := net.Dial("tcp", cfg.BackendAddr)
	if err != nil {
		if nextState == 2 {
			jsonMsg := fmt.Sprintf(`{"text": "%s"}`, escapeJSONString(cfg.Messages.WorldLoading))
			WriteDisconnectPacket(clientConn, jsonMsg)
		}
		if tcpConn, ok := clientConn.(*net.TCPConn); ok {
			tcpConn.CloseWrite()
		}
		time.Sleep(100 * time.Millisecond)
		return
	}
	defer backendConn.Close()

	var handshake bytes.Buffer
	WriteVarIntToBuffer(&handshake, 0x00)
	WriteVarIntToBuffer(&handshake, protocolVersion)

	addrBytes := []byte(serverAddress)
	WriteVarIntToBuffer(&handshake, len(addrBytes))
	handshake.Write(addrBytes)

	handshake.Write(portBuf)
	WriteVarIntToBuffer(&handshake, nextState)

	// Combine length and handshake payload into a single buffer to avoid TCP fragmentation delay
	var finalHandshake bytes.Buffer
	WriteVarIntToBuffer(&finalHandshake, handshake.Len())
	finalHandshake.Write(handshake.Bytes())
	backendConn.Write(finalHandshake.Bytes())

	if nextState == 2 {
		activeMutex.Lock()
		activePlayers++
		if shutdownTimer != nil {
			logVerbose("[SYSTEM] Player joined. Canceling shutdown timer.")
			shutdownTimer.Stop()
			shutdownTimer = nil
		}
		activeMutex.Unlock()
	}

	defer func() {
		if nextState == 2 {
			logVerbose("[SYSTEM] Player %s disconnected.", clientConn.RemoteAddr().String())
			activeMutex.Lock()
			activePlayers--
			// Start the idle timer only if the last player leaves
			if activePlayers == 0 && isServerRunning {
				startCountdown()
			}
			activeMutex.Unlock()
		}
	}()

	// Pipe the connection bi-directionally
	done := make(chan struct{}, 2)
	go func() { io.Copy(backendConn, clientConn); done <- struct{}{} }()
	go func() { io.Copy(clientConn, backendConn); done <- struct{}{} }()
	<-done
}

func startCountdown() {
	logInfo("[SYSTEM] No players online. Starting %d-minute shutdown timer...", cfg.IdleMinutes)

	shutdownTimer = time.AfterFunc(time.Duration(cfg.IdleMinutes)*time.Minute, func() {
		activeMutex.Lock()
		if activePlayers > 0 {
			// A player joined right as the timer triggered, abort shutdown
			activeMutex.Unlock()
			return
		}
		activeMutex.Unlock()

		serverMutex.Lock()
		if isServerRunning && serverStdin != nil {
			logInfo("[SYSTEM] Idle timer reached. Stopping server gracefully...")
			serverStdin.Write([]byte("stop\n"))
		}
		serverMutex.Unlock()
	})
}

func startServer() {
	logInfo("[SYSTEM] Executing Java process...")

	// Split java_args by spaces to support multiple arguments
	args := strings.Fields(cfg.JavaArgs)
	args = append(args, "-jar", cfg.JarFile, "nogui")
	
	logVerbose("Running command: java %v in %s", args, cfg.JarPath)

	cmd := exec.Command("java", args...)
	cmd.Dir = cfg.JarPath

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	var err error
	serverStdin, err = cmd.StdinPipe()
	if err != nil {
		logInfo("[SYSTEM ERROR] Failed to create stdin pipe: %v", err)
		return
	}

	err = cmd.Start()
	if err != nil {
		logInfo("[SYSTEM ERROR] Failed to start server: %v", err)
		serverMutex.Lock()
		isServerRunning = false
		serverMutex.Unlock()
		return
	}

	logInfo("[SYSTEM] Server process launched in background!")

	activeMutex.Lock()
	if activePlayers == 0 {
		startCountdown()
	}
	activeMutex.Unlock()

	go func() {
		cmd.Wait()
		logInfo("[SYSTEM] Server has STOPPED.")

		serverMutex.Lock()
		isServerRunning = false
		serverStdin = nil
		if shutdownTimer != nil {
			shutdownTimer.Stop()
			shutdownTimer = nil
		}
		serverMutex.Unlock()
	}()
}
