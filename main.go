package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"
)

const (
	ManagerPort = "0.0.0.0:25565"
	BackendAddr = "127.0.0.1:25566"
	JarPath     = "/home/ivan/server"
	JarFile     = "paper.jar"
	JavaXms     = "-Xms2G"
	JavaXmx     = "-Xmx2G"
	IdleMinutes = 20
)

var (
	isServerRunning bool
	serverMutex     sync.Mutex
	serverStdin     io.WriteCloser

	activePlayers int
	activeMutex   sync.Mutex
	shutdownTimer *time.Timer
)

func main() {
	listener, err := net.Listen("tcp", ManagerPort)
	if err != nil {
		fmt.Printf("Failed to bind port 25565: %v\n", err)
		os.Exit(1)
	}
	defer listener.Close()

	fmt.Printf("minecraft-od v0.1 by ivanpena\nlistening on %s...\n", ManagerPort)

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
				fmt.Println("[SYSTEM] Command ignored: The server is offline.")
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
	defer conn.Close()  // close

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

		switch nextState {
		case 1:
			// menu refresh/ping
			handleStatus(conn)
		case 2:
			// join server
			if isServerRunning {
				// Reconstruct the handshake and proxy the connection if the server is up
				handleProxy(conn, protocolVersion, serverAddress, portBuf)
			} else {
				handleLoginKick(conn, protocolVersion)
			}
		}
	}
}

// handleStatus: handles the status request and response (ping/pong)
func handleStatus(conn net.Conn) {
	// Enforce a deadline so ping requests don't block forever if the client drops
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// The client sends a Request packet (Length 1, ID 0x00) immediately after Handshake
	_, _ = ReadVarInt(conn)
	_, _ = ReadVarInt(conn)

	var jsonResponse string

	if isServerRunning {
		// Server is active, display online players
		activeMutex.Lock()
		players := activePlayers
		activeMutex.Unlock()

		jsonResponse = fmt.Sprintf(`{
			"version": { "name": "26.1.2", "protocol": 775 },
			"players": { "max": 20, "online": %d },
			"description": { "text": "§a[Minecraft-OD] §7Status: §aOnline\n§7Server is ready!" }
		}`, players)
	} else {
		// Server is asleep, hide player max to indicate offline status
		jsonResponse = `{
			"version": { "name": "§cOffline", "protocol": -1 }, 
			"players": { "max": 0, "online": 0,
				"sample": [
					{"name": "§cServer is sleeping", "id": "00000000-0000-0000-0000-000000000000"},
					{"name": "§aJoin to wake it up!", "id": "00000000-0000-0000-0000-000000000001"}
				]
			},
			"description": { "text": "§e[Minecraft-OD] §7Status: §cSleeping\n§7Join the server to start it up" }
		}`
	}

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
	fmt.Printf("--> Player from %s attempted to join! (Protocol: %d)\n", conn.RemoteAddr().String(), protocolVersion)

	// kick player and show message
	jsonMsg := `{"text": "§e[Minecraft on Demand] §fThe server is booting up.\n§7Please wait ~40 seconds and try again."}`
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

func handleProxy(clientConn net.Conn, protocolVersion int, serverAddress string, portBuf []byte) {
	// rebuild initial handshake

	backendConn, err := net.Dial("tcp", BackendAddr)
	if err != nil {
		jsonMsg := `{"text": "§e[Minecraft on Demand] §fThe world is still loading.\n§7Give it just a few more seconds."}`
		WriteDisconnectPacket(clientConn, jsonMsg)
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
	WriteVarIntToBuffer(&handshake, 2)

	WriteVarInt(backendConn, handshake.Len())
	backendConn.Write(handshake.Bytes())

	activeMutex.Lock()
	activePlayers++
	if shutdownTimer != nil {
		fmt.Println("[SYSTEM] Player joined. Canceling shutdown timer.")
		shutdownTimer.Stop()
		shutdownTimer = nil
	}
	activeMutex.Unlock()

	defer func() {
		fmt.Printf("[SYSTEM] Player %s disconnected.\n", clientConn.RemoteAddr().String())
		activeMutex.Lock()
		activePlayers--
		// Start the idle timer only if the last player leaves
		if activePlayers == 0 && isServerRunning {
			startCountdown()
		}
		activeMutex.Unlock()
	}()

	// Pipe the connection bi-directionally
	done := make(chan struct{}, 2)
	go func() { io.Copy(backendConn, clientConn); done <- struct{}{} }()
	go func() { io.Copy(clientConn, backendConn); done <- struct{}{} }()
	<-done
}

func startCountdown() {
	fmt.Printf("[SYSTEM] No players online. Starting %d-minute shutdown timer...\n", IdleMinutes)

	shutdownTimer = time.AfterFunc(time.Duration(IdleMinutes)*time.Minute, func() {
		activeMutex.Lock()
		if activePlayers > 0 {
			// A player joined right as the timer triggered, abort shutdown
			activeMutex.Unlock()
			return
		}
		activeMutex.Unlock()

		serverMutex.Lock()
		if isServerRunning && serverStdin != nil {
			fmt.Println("[SYSTEM] Idle timer reached. Stopping server gracefully...")
			serverStdin.Write([]byte("stop\n"))
		}
		serverMutex.Unlock()
	})
}

func startServer() {
	fmt.Println("[SYSTEM] Executing Java process...")

	cmd := exec.Command("java", JavaXms, JavaXmx, "-jar", JarFile, "nogui")
	cmd.Dir = JarPath

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	var err error
	serverStdin, err = cmd.StdinPipe()
	if err != nil {
		fmt.Printf("[SYSTEM ERROR] Failed to create stdin pipe: %v\n", err)
		return
	}

	err = cmd.Start()
	if err != nil {
		fmt.Printf("[SYSTEM ERROR] Failed to start server: %v\n", err)
		serverMutex.Lock()
		isServerRunning = false
		serverMutex.Unlock()
		return
	}

	fmt.Println("[SYSTEM] Server process launched in background!")

	activeMutex.Lock()
	if activePlayers == 0 {
		startCountdown()
	}
	activeMutex.Unlock()

	go func() {
		cmd.Wait()
		fmt.Println("[SYSTEM] Server has STOPPED.")

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
