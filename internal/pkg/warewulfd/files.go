package warewulfd

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	warewulfconf "github.com/warewulf/warewulf/internal/pkg/config"
	"github.com/warewulf/warewulf/internal/pkg/node"
	"github.com/warewulf/warewulf/internal/pkg/overlay"
	"github.com/warewulf/warewulf/internal/pkg/wwlog"
)

var (
	// errInvalidPeerAddress indicates the request RemoteAddr could not be parsed.
	errInvalidPeerAddress = errors.New("could not parse remote address")
	// errNodeUnidentified indicates that neither ?wwid= nor the ARP cache
	// could identify the requesting node.
	errNodeUnidentified = errors.New("unable to identify node")
)

// noListFileSystem wraps http.FileSystem and returns os.ErrNotExist for
// directories, disabling directory listing.
type noListFileSystem struct {
	http.FileSystem
}

func (fsys noListFileSystem) Open(name string) (http.File, error) {
	f, err := fsys.FileSystem.Open(name)
	if err != nil {
		return nil, err
	}

	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	if stat.IsDir() {
		_ = f.Close()
		return nil, os.ErrNotExist
	}

	return f, nil
}

// resolveFilesPath safely resolves a request path to a file within filesDir.
// Returns the clean absolute path or an empty string if the path escapes filesDir.
func resolveFilesPath(filesDir string, reqPath string) string {
	// Strip the /files/ prefix
	relPath := strings.TrimPrefix(reqPath, "/files/")
	if relPath == "" {
		return ""
	}

	fullPath := filepath.Join(filesDir, relPath)
	cleanPath := filepath.Clean(fullPath)
	cleanDir := filepath.Clean(filesDir)

	rel, err := filepath.Rel(cleanDir, cleanPath)
	if err != nil {
		return ""
	}
	if strings.HasPrefix(rel, "..") {
		return ""
	}

	return cleanPath
}

// identifyRequest determines the peer address, peer port and hardware
// address of the requesting node. The hardware address comes from the
// ?wwid= query parameter with an ARP cache fallback. It performs no node
// database lookup itself, so it is independent of any configuration
// generation.
func identifyRequest(req *http.Request) (ipaddr string, remoteport int, hwaddr string, err error) {
	remoteAddrPort, err := netip.ParseAddrPort(req.RemoteAddr)
	if err != nil {
		wwlog.Error("could not parse remote address: %s", req.RemoteAddr)
		return "", 0, "", fmt.Errorf("%w: %s", errInvalidPeerAddress, req.RemoteAddr)
	}
	ipaddr = remoteAddrPort.Addr().String()
	remoteport = int(remoteAddrPort.Port())

	if wwids := req.URL.Query()["wwid"]; len(wwids) > 0 {
		hwaddr = parseHwaddr(wwids[0])
	}
	if hwaddr == "" {
		if mac := parseHwaddr(ArpFind(ipaddr)); mac != "" {
			hwaddr = mac
			wwlog.Verbose("using %s from arp cache for %s", hwaddr, ipaddr)
		}
	}
	if hwaddr == "" {
		wwlog.Denied("unable to identify node for %s", ipaddr)
		return ipaddr, remoteport, "", errNodeUnidentified
	}
	return ipaddr, remoteport, hwaddr, nil
}

// checkSecureFilesPort enforces the secure files privileged-port policy.
// The policy comes from warewulf.conf and is independent of the node
// configuration generation.
func checkSecureFilesPort(w http.ResponseWriter, req *http.Request, remoteport int) bool {
	if warewulfconf.Get().Warewulf.SecureFiles() && remoteport >= 1024 {
		wwlog.Denied("non-privileged port: %s", req.RemoteAddr)
		http.Error(w, "non-privileged port", http.StatusForbidden)
		return false
	}
	return true
}

// checkNodeAssetKey enforces the node asset key for a request. The asset key
// must come from the same configuration generation as everything else used
// to serve the request.
func checkNodeAssetKey(w http.ResponseWriter, req *http.Request, remoteNode node.Node) bool {
	if remoteNode.AssetKey == "" {
		return true
	}
	assetkey := ""
	if keys := req.URL.Query()["assetkey"]; len(keys) > 0 {
		assetkey = keys[0]
	}
	if assetkey == "" {
		wwlog.Denied("missing asset key for node %s", remoteNode.Id())
		http.Error(w, "asset key required", http.StatusUnauthorized)
		return false
	}
	if assetkey != remoteNode.AssetKey {
		wwlog.Denied("incorrect asset key for node %s", remoteNode.Id())
		http.Error(w, "incorrect asset key", http.StatusForbidden)
		return false
	}
	return true
}

// authenticateConfiguredNode authenticates a static-file request against the
// in-memory NodeDB and the warewulf.conf secure-files policy.
func authenticateConfiguredNode(w http.ResponseWriter, req *http.Request, remoteport int, hwaddr string) bool {
	remoteNode, err := GetNode(hwaddr)
	if err != nil {
		wwlog.Denied("node not found for hwaddr %s: %s", hwaddr, err)
		http.Error(w, "node not found", http.StatusUnauthorized)
		return false
	}

	if !checkSecureFilesPort(w, req, remoteport) {
		return false
	}

	if !checkNodeAssetKey(w, req, remoteNode) {
		return false
	}
	return true
}

// HandleFiles serves static files from the configured warewulf files directory.
// Every request must identify a node via ?wwid= or ARP fallback.
// If the node has an asset key, ?assetkey= must match.
// When secure files mode is enabled, requests must come from a privileged port.
// If ?render is present, the file is rendered as a Go template for the
// identified node, using one immutable snapshot of the node configuration and
// template content: authentication, target node, allNodes, profile field
// sources and rendered bytes all belong to that snapshot. If the path does not
// end in .ww but a .ww-suffixed version exists, that file is used.
func HandleFiles(w http.ResponseWriter, req *http.Request) {
	conf := warewulfconf.Get()
	filesDir := conf.Paths.WWFilesdir
	wwlog.Debug("Serving file from %s: %s", filesDir, req.URL.Path)

	ipaddr, remoteport, hwaddr, err := identifyRequest(req)
	if err != nil {
		if errors.Is(err, errInvalidPeerAddress) {
			http.Error(w, "could not parse remote address", http.StatusInternalServerError)
		} else {
			http.Error(w, "unable to identify node", http.StatusUnauthorized)
		}
		return
	}

	if _, render := req.URL.Query()["render"]; render {
		handleFilesRender(w, req, filesDir, ipaddr, remoteport, hwaddr)
		return
	}

	if !authenticateConfiguredNode(w, req, remoteport, hwaddr) {
		return
	}

	fs := noListFileSystem{http.Dir(filesDir)}
	http.StripPrefix("/files/", http.FileServer(fs)).ServeHTTP(w, req)
}

// selectRenderTemplate resolves the requested path to an existing template,
// applying the .ww suffix fallback. It writes the HTTP error response and
// returns an empty path on failure.
func selectRenderTemplate(w http.ResponseWriter, filesDir string, req *http.Request) string {
	filePath := resolveFilesPath(filesDir, req.URL.Path)
	if filePath == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return ""
	}

	if !strings.HasSuffix(filePath, ".ww") {
		wwFilePath := filePath + ".ww"
		if _, err := os.Stat(wwFilePath); err == nil {
			wwlog.Debug("files: using .ww suffix for %s", filePath)
			filePath = wwFilePath
		} else if _, err := os.Stat(filePath); err == nil {
			http.Error(w, "render requires a .ww template file", http.StatusBadRequest)
			return ""
		} else {
			http.Error(w, "not found", http.StatusNotFound)
			return ""
		}
	} else if _, err := os.Stat(filePath); os.IsNotExist(err) {
		http.Error(w, "not found", http.StatusNotFound)
		return ""
	}
	return filePath
}

// handleFilesRender renders a template for the identified node. The request
// is bound to a single snapshot acquired before any authentication data is
// used: a later publication of nodes.conf or of the template only affects
// requests that start afterwards. Failures of the three distinct stages
// (node configuration unavailable, template replaced during the read,
// client disappearing during the write) terminate this generation without
// falling back to another one.
func handleFilesRender(w http.ResponseWriter, req *http.Request, filesDir, ipaddr string, remoteport int, hwaddr string) {
	snap, err := newFilesSnapshot()
	if err != nil {
		wwlog.Error("files: %s", err)
		http.Error(w, "could not initialize request snapshot", http.StatusInternalServerError)
		return
	}
	defer snap.remove()

	// Pinned node configuration: nodes.conf is read once for this request.
	// Authentication, target node, allNodes and profile field sources all
	// derive from these bytes.
	if err := snap.loadNodesConf(); err != nil {
		wwlog.Error("files: %s", err)
		if errors.Is(err, errNodesConfUnavailable) {
			http.Error(w, "node configuration unavailable", http.StatusInternalServerError)
		} else {
			http.Error(w, "error parsing node configuration", http.StatusInternalServerError)
		}
		return
	}

	remoteNode, err := snap.lookupNode(hwaddr)
	if err != nil {
		wwlog.Denied("node not found for hwaddr %s: %s", hwaddr, err)
		http.Error(w, "node not found", http.StatusUnauthorized)
		return
	}

	// The secure-files policy is warewulf.conf state, not node state.
	if !checkSecureFilesPort(w, req, remoteport) {
		return
	}

	if !checkNodeAssetKey(w, req, remoteNode) {
		return
	}

	if renderName := req.URL.Query().Get("render"); renderName != "" && renderName != remoteNode.Id() {
		http.Error(w, fmt.Sprintf("render node %q does not match identified node %q", renderName, remoteNode.Id()), http.StatusBadRequest)
		return
	}

	filePath := selectRenderTemplate(w, filesDir, req)
	if filePath == "" {
		return
	}

	// Pin the template bytes. A replacement during the read fails the
	// request instead of rendering a mixed or half-written generation.
	if err := snap.stageTemplate(filePath); err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			wwlog.Error("files: template disappeared before it could be read: %s", filePath)
			http.Error(w, "not found", http.StatusNotFound)
		case errors.Is(err, errFileChangedDuringRead):
			wwlog.Error("files: template %s changed during read; failing request without using another generation", filePath)
			http.Error(w, "template file changed during read", http.StatusInternalServerError)
		default:
			wwlog.Error("files: error reading template %s: %s", filePath, err)
			http.Error(w, fmt.Sprintf("error reading template: %s", err), http.StatusInternalServerError)
		}
		return
	}

	allNodes, err := snap.allNodes()
	if err != nil {
		wwlog.Error("files: error loading nodes: %s", err)
		http.Error(w, fmt.Sprintf("error loading nodes: %s", err), http.StatusInternalServerError)
		return
	}

	tstruct, err := overlay.InitStruct("", remoteNode, allNodes)
	if err != nil {
		wwlog.Error("files: error initializing template data: %s", err)
		http.Error(w, fmt.Sprintf("error initializing template data: %s", err), http.StatusInternalServerError)
		return
	}
	tstruct.BuildSource = filePath

	buffer, _, _, err := overlay.RenderTemplateFile(snap.stagedTemplatePath, tstruct)
	if err != nil {
		wwlog.Error("files: error rendering template %s: %s", filePath, err)
		http.Error(w, fmt.Sprintf("error rendering template: %s", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(buffer.Len()))
	if _, err := buffer.WriteTo(w); err != nil {
		// The response (and possibly its headers) was already partially
		// sent; an error status cannot be written. Log the interrupted
		// write and stop without retrying through another generation.
		wwlog.Warn("files: client %s disconnected before receiving rendered %s for node %s: %s", ipaddr, filePath, remoteNode.Id(), err)
		return
	}
	wwlog.Info("files: rendered %s for node %s", filePath, remoteNode.Id())
}
