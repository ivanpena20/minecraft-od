
# minecraft-od (Minecraft on Demand)

tiny program to automatically start and stop a minecraft server

It basically works by reading the packet the minecraft client sends to the server when joining and if the main server is off, it starts it and redirects all new packets from client to the main server port. If no one is online within a specified time, the server shuts down itself, and the loop repeats.

## Changelog

### v0.0 (2026-06-10)
- prototype

### v0.1 (2026-06-11)
- first release
- cleaned up code
- added comments
- fixed console input

### v0.2 (2026-06-11)
- revamped tcp connection handler
- added shutdown/ctrl-c protection
- added configuration
- made online motd passthrough
- server-icon support

