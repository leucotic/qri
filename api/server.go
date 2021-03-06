package api

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/rpc"
	"time"

	"github.com/datatogether/api/apiutil"
	"github.com/ipfs/go-datastore"
	golog "github.com/ipfs/go-log"
	"github.com/qri-io/cafs"
	"github.com/qri-io/qri/config"
	"github.com/qri-io/qri/lib"
	"github.com/qri-io/qri/p2p"
	"github.com/qri-io/qri/repo"
)

var log = golog.Logger("qriapi")

func init() {
	golog.SetLogLevel("qriapi", "info")
}

// Server wraps a qri p2p node, providing traditional access via http
// Create one with New, start it up with Serve
type Server struct {
	// configuration options
	cfg     *config.Config
	qriNode *p2p.QriNode
}

// New creates a new qri server with optional configuration
// calling New() with no options will return the default configuration
// as specified in DefaultConfig
func New(r repo.Repo, options ...func(*config.Config)) (s *Server, err error) {
	cfg := config.DefaultConfig()
	for _, opt := range options {
		opt(cfg)
	}

	s = &Server{
		cfg: cfg,
	}

	// allocate a new node
	s.qriNode, err = p2p.NewQriNode(r, func(c *config.P2P) {
		*c = *cfg.P2P
	})
	if err != nil {
		return s, err
	}

	return s, nil
}

// Serve starts the server. It will block while the server is running
func (s *Server) Serve() (err error) {
	server := &http.Server{}
	server.Handler = NewServerRoutes(s)

	go s.ServeRPC()
	go s.ServeWebapp()

	// func(p2pcfg *config.P2P) {
	// p2pcfg.Online = s.cfg.Online
	// if cfg.BoostrapAddrs != nil {
	// 	p2pcfg.QriBootstrapAddrs = cfg.BoostrapAddrs
	// }
	// })

	// bootstrapped := false
	peerBootstrapped := func(peerId string) {
		// if cfg.PostP2POnlineHook != nil && !bootstrapped {
		// go cfg.PostP2POnlineHook(s.qriNode)
		// bootstrapped = true
		// }
	}

	err = s.qriNode.StartOnlineServices(peerBootstrapped)
	if err != nil {
		return fmt.Errorf("error starting P2P service: %s", err.Error())
	}

	if node, err := s.qriNode.IPFSNode(); err == nil {
		if pinner, ok := s.qriNode.Repo.Store().(cafs.Pinner); ok {

			go func() {
				if _, err := lib.CheckVersion(context.Background(), node.Namesys, lib.PrevIPNSName, lib.LastPubVerHash); err == lib.ErrUpdateRequired {
					log.Info("This version of qri is out of date, please refer to https://github.com/qri-io/qri/releases/latest for more info")
				} else if err != nil {
					log.Infof("error checking for software update: %s", err.Error())
				}
			}()

			go func() {
				// TODO - this is breaking encapsulation pretty hard. Should probs move this stuff into lib
				if lib.Config != nil && lib.Config.Render != nil && lib.Config.Render.TemplateUpdateAddress != "" {
					if latest, err := lib.CheckVersion(context.Background(), node.Namesys, lib.Config.Render.TemplateUpdateAddress, lib.Config.Render.DefaultTemplateHash); err == lib.ErrUpdateRequired {
						err := pinner.Pin(datastore.NewKey(latest), true)
						if err != nil {
							log.Debug("error pinning template hash: %s", err.Error())
							return
						}
						if err := lib.Config.Set("Render.DefaultTemplateHash", latest); err != nil {
							log.Debug("error setting latest hash: %s", err.Error())
							return
						}
						if err := lib.SaveConfig(); err != nil {
							log.Debug("error saving config hash: %s", err.Error())
							return
						}
						log.Info("auto-updated template hash: %s", latest)
					}
				}
			}()

		}
	}

	info := s.cfg.SummaryString()
	info += "IPFS Addresses:"
	for _, a := range s.qriNode.EncapsulatedAddresses() {
		info = fmt.Sprintf("%s\n  %s", info, a.String())
	}
	log.Info(info)

	if s.cfg.API.DisconnectAfter != 0 {
		log.Infof("disconnecting after %d seconds", s.cfg.API.DisconnectAfter)
		go func(s *http.Server, t int) {
			<-time.After(time.Second * time.Duration(t))
			log.Infof("disconnecting")
			s.Close()
		}(server, s.cfg.API.DisconnectAfter)
	}

	// http.ListenAndServe will not return unless there's an error
	return StartServer(s.cfg.API, server)
}

// ServeRPC checks for a configured RPC port, and registers a listner if so
func (s *Server) ServeRPC() {
	if !s.cfg.RPC.Enabled || s.cfg.RPC.Port == 0 {
		return
	}

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", s.cfg.RPC.Port))
	if err != nil {
		log.Infof("RPC listen on port %d error: %s", s.cfg.RPC.Port, err)
		return
	}

	for _, rcvr := range lib.Receivers(s.qriNode) {
		if err := rpc.Register(rcvr); err != nil {
			log.Infof("error registering RPC receiver %s: %s", rcvr.CoreRequestsName(), err.Error())
			return
		}
	}

	rpc.Accept(listener)
	return
}

// HandleIPFSPath responds to IPFS Hash requests with raw data
func (s *Server) HandleIPFSPath(w http.ResponseWriter, r *http.Request) {
	if s.cfg.API.ReadOnly {
		readOnlyResponse(w, "/ipfs/")
		return
	}

	s.fetchIPFSPath(r.URL.Path, w, r)
}

func (s *Server) fetchIPFSPath(path string, w http.ResponseWriter, r *http.Request) {
	file, err := s.qriNode.Repo.Store().Get(datastore.NewKey(path))
	if err != nil {
		apiutil.WriteErrResponse(w, http.StatusInternalServerError, err)
		return
	}

	io.Copy(w, file)
}

// HandleIPNSPath resolves an IPNS entry
func (s *Server) HandleIPNSPath(w http.ResponseWriter, r *http.Request) {
	if s.cfg.API.ReadOnly {
		readOnlyResponse(w, "/ipns/")
		return
	}

	node, err := s.qriNode.IPFSNode()
	if err != nil {
		apiutil.WriteErrResponse(w, http.StatusBadRequest, fmt.Errorf("no IPFS node present: %s", err.Error()))
		return
	}

	p, err := node.Namesys.Resolve(r.Context(), r.URL.Path[len("/ipns/"):])
	if err != nil {
		apiutil.WriteErrResponse(w, http.StatusBadRequest, fmt.Errorf("error resolving IPNS Name: %s", err.Error()))
		return
	}

	file, err := s.qriNode.Repo.Store().Get(datastore.NewKey(p.String()))
	if err != nil {
		apiutil.WriteErrResponse(w, http.StatusInternalServerError, err)
		return
	}

	io.Copy(w, file)
}

// helper function
func readOnlyResponse(w http.ResponseWriter, endpoint string) {
	apiutil.WriteErrResponse(w, http.StatusForbidden, fmt.Errorf("qri server is in read-only mode, access to '%s' endpoint is forbidden", endpoint))
}

// HealthCheckHandler is a basic ok response for load balancers & co
// returns the version of qri this node is running, pulled from the lib package
func HealthCheckHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{ "meta": { "code": 200, "status": "ok", "version":"` + lib.VersionNumber + `" }, "data": [] }`))
}

// NewServerRoutes returns a Muxer that has all API routes
func NewServerRoutes(s *Server) *http.ServeMux {
	m := http.NewServeMux()

	m.Handle("/status", s.middleware(HealthCheckHandler))
	m.Handle("/ipfs/", s.middleware(s.HandleIPFSPath))
	m.Handle("/ipns/", s.middleware(s.HandleIPNSPath))

	proh := NewProfileHandlers(s.qriNode.Repo, s.cfg.API.ReadOnly)
	m.Handle("/me", s.middleware(proh.ProfileHandler))
	m.Handle("/profile", s.middleware(proh.ProfileHandler))
	m.Handle("/profile/photo", s.middleware(proh.ProfilePhotoHandler))
	m.Handle("/profile/poster", s.middleware(proh.PosterHandler))

	ph := NewPeerHandlers(s.qriNode.Repo, s.qriNode, s.cfg.API.ReadOnly)
	m.Handle("/peers", s.middleware(ph.PeersHandler))
	m.Handle("/peers/", s.middleware(ph.PeerHandler))

	m.Handle("/connect/", s.middleware(ph.ConnectToPeerHandler))
	m.Handle("/connections", s.middleware(ph.ConnectionsHandler))

	dsh := NewDatasetHandlers(s.qriNode.Repo, s.cfg.API.ReadOnly)

	// TODO - stupid hack for now.
	dsh.DatasetRequests.Node = s.qriNode
	m.Handle("/list", s.middleware(dsh.ListHandler))
	m.Handle("/list/", s.middleware(dsh.PeerListHandler))
	m.Handle("/save", s.middleware(dsh.SaveHandler))
	m.Handle("/save/", s.middleware(dsh.SaveHandler))
	m.Handle("/remove/", s.middleware(dsh.RemoveHandler))
	m.Handle("/me/", s.middleware(dsh.GetHandler))
	m.Handle("/add", s.middleware(dsh.InitHandler))
	m.Handle("/add/", s.middleware(dsh.AddHandler))
	m.Handle("/rename", s.middleware(dsh.RenameHandler))
	m.Handle("/export/", s.middleware(dsh.ZipDatasetHandler))
	m.Handle("/diff", s.middleware(dsh.DiffHandler))
	m.Handle("/body/", s.middleware(dsh.BodyHandler))

	hh := NewHistoryHandlers(s.qriNode.Repo)
	// TODO - stupid hack for now.
	hh.HistoryRequests.Node = s.qriNode
	m.Handle("/history/", s.middleware(hh.LogHandler))

	rgh := NewRegistryHandlers(s.qriNode.Repo)
	m.Handle("/registry/", s.middleware(rgh.RegistryHandler))

	sh := NewSearchHandlers(s.qriNode.Repo)
	m.Handle("/search", s.middleware(sh.SearchHandler))

	rh := NewRootHandler(dsh, ph)
	m.Handle("/", s.datasetRefMiddleware(s.middleware(rh.Handler)))

	return m
}
