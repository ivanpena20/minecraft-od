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

// Variables globales
var (
	isServerRunning bool
	serverMutex     sync.Mutex
	serverStdin     io.WriteCloser

	activePlayers int
	activeMutex   sync.Mutex
	shutdownTimer *time.Timer
)

func main() {
	listener, err := net.Listen("tcp", "0.0.0.0:25565")
	if err != nil {
		fmt.Printf("Failed to bind port 25565: %v\n", err)
		os.Exit(1)
	}
	defer listener.Close()

	fmt.Println("minecraft-od v0.1 by ivanpena\nlistening on port 25565...")

	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			input := scanner.Text() + "\n"

			serverMutex.Lock()
			if isServerRunning && serverStdin != nil {
				// if server is running, send the command (ej: "op ivanpena")
				serverStdin.Write([]byte(input))
			} else {
				// If the server is offline, notify the user not to write to the void
				fmt.Println("[SYSTEM] Command ignored: The Java server is offline.")
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

// handleConnection: Recepcionista inteligente
func handleConnection(conn net.Conn) {
	defer conn.Close()

	// Timeout de 3 segundos para evitar que conexiones fantasma cuelguen el hilo
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	_, err := ReadVarInt(conn)
	if err != nil {
		return
	}

	packetID, err := ReadVarInt(conn)
	if err != nil {
		return
	}
	if packetID == 0xFE {
		return
	}

	if packetID == 0x00 {
		// GUARDAMOS los datos del Handshake en lugar de ignorarlos
		protocolVersion, _ := ReadVarInt(conn)
		serverAddress, _ := ReadString(conn)
		portBuf := make([]byte, 2)
		io.ReadFull(conn, portBuf)

		nextState, _ := ReadVarInt(conn)

		// IMPORTANTE: Quitamos el límite de 3 segundos porque ya hemos leído la intención.
		// Si no lo quitamos, el proxy le cortará la conexión al jugador a los 3 segundos de entrar.
		conn.SetReadDeadline(time.Time{})

		switch nextState {
		case 1:
			// El jugador solo está mirando la lista de servidores (Ping)
			handleStatus(conn)
		case 2:
			// El jugador quiere entrar a jugar
			if isServerRunning {
				// Pasamos los datos que leímos para poder reconstruir el paquete
				handleProxy(conn, protocolVersion, serverAddress, portBuf)
			} else {
				handleLoginKick(conn, protocolVersion)
			}
		}
	}
}

// handleStatus: Ahora el MOTD es dinámico
func handleStatus(conn net.Conn) {
	_, _ = ReadVarInt(conn)
	_, _ = ReadVarInt(conn)

	var jsonResponse string

	if isServerRunning {
		// Servidor ENCENDIDO: Mostramos cuánta gente hay jugando
		activeMutex.Lock()
		players := activePlayers
		activeMutex.Unlock()

		jsonResponse = fmt.Sprintf(`{
			"version": { "name": "26.1.2", "protocol": 775 },
			"players": { "max": 20, "online": %d },
			"description": { "text": "§a[Minecraft-OD] §7Estado: §aEncendido\n§7¡Servidor listo para jugar!" }
		}`, players)
	} else {
		// Servidor APAGADO: Mostramos el mensaje de dormir
		jsonResponse = `{
			"version": { "name": "26.1.2 (Apagado)", "protocol": 775 },
			"players": { "max": 20, "online": 0,
				"sample": [
					{"name": "§cServidor apagado", "id": "00000000-0000-0000-0000-000000000000"},
					{"name": "§aEntra para encenderlo", "id": "00000000-0000-0000-0000-000000000001"}
				]
			},
			"description": { "text": "§e[Minecraft-OD] §7Estado: §cApagado\n§7Entra para encender el servidor" }
		}`
	}

	WriteStatusResponse(conn, jsonResponse)

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

	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.CloseWrite()
	}
	time.Sleep(50 * time.Millisecond)
}

func handleLoginKick(conn net.Conn, protocolVersion int) {
	fmt.Printf("--> Player from %s attempted to join! (Protocol: %d)\n", conn.RemoteAddr().String(), protocolVersion)

	jsonMsg := `{"text": "§e[Minecraft on Demand] §fEl servidor principal está arrancando.\n§7Espera unos 40 segundos y vuelve a intentarlo."}`
	WriteDisconnectPacket(conn, jsonMsg)

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

// handleProxy: Enlaza la conexión, reconstruyendo el paquete perdido
func handleProxy(clientConn net.Conn, protocolVersion int, serverAddress string, portBuf []byte) {
	// IMPORTANTE: Al haber leído el Handshake en Go para ver el nextState,
	// Java no va a recibir esa presentación. Tenemos que reconstruirla a mano y enviársela.

	backendConn, err := net.Dial("tcp", "127.0.0.1:25566")
	if err != nil {
		jsonMsg := `{"text": "§e[Minecraft on Demand] §fSe está terminando de cargar el mundo.\n§7Espera unos segundos más."}`
		WriteDisconnectPacket(clientConn, jsonMsg)
		if tcpConn, ok := clientConn.(*net.TCPConn); ok {
			tcpConn.CloseWrite()
		}
		time.Sleep(100 * time.Millisecond)
		return
	}
	defer backendConn.Close()

	// --- RECONSTRUIR Y ENVIAR HANDSHAKE ---
	var handshake bytes.Buffer
	WriteVarIntToBuffer(&handshake, 0x00) // Packet ID
	WriteVarIntToBuffer(&handshake, protocolVersion)

	addrBytes := []byte(serverAddress)
	WriteVarIntToBuffer(&handshake, len(addrBytes))
	handshake.Write(addrBytes)

	handshake.Write(portBuf)
	WriteVarIntToBuffer(&handshake, 2) // Next State (2 = Login)

	// Enviamos la longitud total y luego el paquete reconstruido a Java
	WriteVarInt(backendConn, handshake.Len())
	backendConn.Write(handshake.Bytes())
	// ---------------------------------------

	// --- LÓGICA DEL TEMPORIZADOR SANA ---
	activeMutex.Lock()
	activePlayers++
	if shutdownTimer != nil {
		fmt.Println("[SYSTEM] Player joined. Canceling shutdown timer.")
		shutdownTimer.Stop()
		shutdownTimer = nil
	}
	activeMutex.Unlock()

	defer func() {
		activeMutex.Lock()
		activePlayers--
		// Solo iniciamos cuenta atrás si sale el último jugador real y el servidor sigue encendido
		if activePlayers == 0 && isServerRunning {
			startCountdown()
		}
		activeMutex.Unlock()
	}()
	// ------------------------------------

	// Juntar las tuberías de forma bidireccional
	done := make(chan struct{}, 2)
	go func() { io.Copy(backendConn, clientConn); done <- struct{}{} }()
	go func() { io.Copy(clientConn, backendConn); done <- struct{}{} }()
	<-done
}

// startCountdown: Inicializa el contador de 20 min de inactividad
func startCountdown() {
	minutes := 20
	fmt.Printf("[SYSTEM] No players online. Starting %d-minute shutdown timer...\n", minutes)

	shutdownTimer = time.AfterFunc(time.Duration(minutes)*time.Minute, func() {
		serverMutex.Lock()
		if isServerRunning && serverStdin != nil {
			fmt.Println("[SYSTEM] 20 minutes idle reached. Stopping server gracefully...")
			serverStdin.Write([]byte("stop\n")) // Envía el comando de forma segura
		}
		serverMutex.Unlock()
	})
}

func startServer() {
	fmt.Println("[SYSTEM] Executing Java process...")

	cmd := exec.Command("java", "-Xms2G", "-Xmx2G", "-jar", "paper.jar", "nogui")

	// ¡Asegúrate de poner tu ruta absoluta correcta aquí!
	cmd.Dir = "/home/ivan/server"

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
