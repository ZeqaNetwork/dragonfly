package dragonfly

import (
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/dragonfly-tech/dragonfly/dragonfly/block/encoder"
	"github.com/dragonfly-tech/dragonfly/dragonfly/player"
	"github.com/dragonfly-tech/dragonfly/dragonfly/player/skin"
	"github.com/dragonfly-tech/dragonfly/dragonfly/session"
	"github.com/dragonfly-tech/dragonfly/dragonfly/world"
	"github.com/go-gl/mathgl/mgl32"
	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sirupsen/logrus"
	"log"
	"sync"
)

// Server implements a Dragonfly server. It runs the main server loop and handles the connections of players
// trying to join the server.
type Server struct {
	c        Config
	log      *logrus.Logger
	listener *minecraft.Listener
	players  chan *player.Player
	world    *world.World

	playerMutex sync.RWMutex
	// p holds a map of all players currently connected to the server. When they leave, they are removed from
	// the map.
	p map[uuid.UUID]*player.Player
}

// New returns a new server using the Config passed. If nil is passed, a default configuration is returned.
// (A call to dragonfly.DefaultConfig().)
// The Logger passed will be used to log errors and information to. If nil is passed, a default Logger is
// used by calling logrus.New().
// Note that no two servers should be active at the same time. Doing so anyway will result in unexpected
// behaviour.
func New(c *Config, log *logrus.Logger) *Server {
	if log == nil {
		log = logrus.New()
	}
	s := &Server{
		c:       DefaultConfig(),
		log:     log,
		players: make(chan *player.Player),
		world:   world.New(log),
		p:       make(map[uuid.UUID]*player.Player),
	}
	if c != nil {
		s.c = *c
	}
	return s
}

// Accept accepts an incoming player into the server. It blocks until a player connects to the server.
// Accept returns an error if the Server is closed using a call to Close.
func (server *Server) Accept() (*player.Player, error) {
	p, ok := <-server.players
	if !ok {
		return nil, errors.New("server closed")
	}
	server.playerMutex.Lock()
	server.p[p.UUID()] = p
	server.playerMutex.Unlock()

	return p, nil
}

// World returns the world of the server. Players will be spawned in this world and this world will be read
// from and written to when the world is edited.
func (server *Server) World() *world.World {
	return server.world
}

// Run runs the server and blocks until it is closed using a call to Close(). When called, the server will
// accept incoming connections.
// After a call to Run, calls to Server.Accept() may be made to accept players into the server.
func (server *Server) Run() error {
	if err := server.startListening(); err != nil {
		return err
	}
	server.run()
	return nil
}

// Start runs the server but does not block, unlike Run, but instead accepts connections on a different
// goroutine. Connections will be accepted until the listener is closed using a call to Close.
// One started, players may be accepted using Server.Accept().
func (server *Server) Start() error {
	if err := server.startListening(); err != nil {
		return err
	}
	go server.run()
	return nil
}

// PlayerCount returns the current player count of the server. It is equivalent to calling
// len(server.Players()).
func (server *Server) PlayerCount() int {
	server.playerMutex.RLock()
	defer server.playerMutex.RUnlock()

	return len(server.p)
}

// MaxPlayerCount returns the maximum amount of players that are allowed to play on the server at the same
// time. Players trying to join when the server is full will be refused to enter.
// If the config has a maximum player count set to 0, MaxPlayerCount will return Server.PlayerCount + 1.
func (server *Server) MaxPlayerCount() int {
	if server.c.Server.MaximumPlayers == 0 {
		return server.PlayerCount() + 1
	}
	return server.c.Server.MaximumPlayers
}

// Players returns a list of all players currently connected to the server. Note that the slice returned is
// not updated when new players join or leave, so it is only valid for as long as no new players join or
// players leave.
func (server *Server) Players() []*player.Player {
	server.playerMutex.RLock()
	defer server.playerMutex.RUnlock()

	players := make([]*player.Player, 0, len(server.p))
	for _, p := range server.p {
		players = append(players, p)
	}
	return players
}

// Player looks for a player on the server with the UUID passed. If found, the player is returned and the bool
// returns holds a true value. If not, the bool returned is false and the player is nil.
func (server *Server) Player(uuid uuid.UUID) (*player.Player, bool) {
	server.playerMutex.RLock()
	defer server.playerMutex.RUnlock()

	if p, ok := server.p[uuid]; ok {
		return p, true
	}
	return nil, false
}

// Close closes the server, making any call to Run/Accept cancel immediately.
func (server *Server) Close() error {
	close(server.players)
	_ = server.world.Close()
	return server.listener.Close()
}

// startListening starts making the Minecraft listener listen, accepting new connections from players.
func (server *Server) startListening() error {
	server.log.Info("Starting server...")

	w := server.log.Writer()
	defer func() {
		_ = w.Close()
	}()

	server.listener = &minecraft.Listener{
		// We wrap a log.Logger around our Logrus logger so that it will print in the same format as the
		// normal Logrus logger would.
		ErrorLog:       log.New(w, "", 0),
		ServerName:     server.c.Server.Name,
		MaximumPlayers: server.c.Server.MaximumPlayers,
	}
	if err := server.listener.Listen("raknet", server.c.Network.Address); err != nil {
		return fmt.Errorf("listening on address failed: %v", err)
	}

	server.log.Infof("Server running on %v.\n", server.listener.Addr())
	return nil
}

// run runs the server, continuously accepting new connections from players. It returns when the server is
// closed by a call to Close.
func (server *Server) run() {
	for {
		c, err := server.listener.Accept()
		if err != nil {
			// Accept will only return an error if the Listener was closed, meaning trying to continue
			// listening is futile.
			return
		}
		go server.handleConn(c.(*minecraft.Conn))
	}
}

// handleConn handles an incoming connection accepted from the Listener.
func (server *Server) handleConn(conn *minecraft.Conn) {
	data := minecraft.GameData{
		WorldName:      server.c.World.Name,
		Blocks:         encoder.Blocks,
		PlayerPosition: mgl32.Vec3{0, 10, 0},
		PlayerGameMode: 1,
		// We set these IDs to 1, because that's how the session will treat them.
		EntityUniqueID:  1,
		EntityRuntimeID: 1,
	}
	if err := conn.StartGame(data); err != nil {
		_ = server.listener.Disconnect(conn, "Connection timeout.")
		server.log.Debugf("connection %v failed spawning: %v\n", conn.RemoteAddr(), err)
		return
	}
	id, err := uuid.Parse(conn.IdentityData().Identity)
	if err != nil {
		_ = conn.Close()
		server.log.Warnf("connection %v has a malformed UUID ('%v')\n", conn.RemoteAddr(), id)
		return
	}
	server.createPlayer(id, conn)
}

// handleSessionClose handles the closing of a session. It removes the player of the session from the server.
func (server *Server) handleSessionClose(controllable session.Controllable) {
	server.playerMutex.Lock()
	defer server.playerMutex.Unlock()

	delete(server.p, controllable.UUID())
}

// createPlayer creates a new player instance using the UUID and connection passed.
func (server *Server) createPlayer(id uuid.UUID, conn *minecraft.Conn) {
	p := &player.Player{}
	s := session.New(p, conn, server.world, server.c.World.MaximumChunkRadius, server.log)
	*p = *player.NewWithSession(conn.IdentityData().DisplayName, conn.IdentityData().XUID, id, server.createSkin(conn.ClientData()), s, server.world)
	s.Start(server.handleSessionClose)

	server.players <- p
}

// createSkin creates a new skin using the skin data found in the client data in the login, and returns it.
func (server *Server) createSkin(data login.ClientData) skin.Skin {
	// gophertunnel guarantees the following values are valid base64 data and are of the correct size.
	skinData, _ := base64.StdEncoding.DecodeString(data.SkinData)
	modelData, _ := base64.StdEncoding.DecodeString(data.SkinGeometry)
	playerSkin, _ := skin.NewFromBytes(skinData)
	playerSkin.ID = data.SkinID
	playerSkin.ModelName = data.SkinGeometryName
	playerSkin.Model = modelData

	return playerSkin
}