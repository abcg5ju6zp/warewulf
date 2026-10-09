package warewulfd

import (
	"net/http"
	"path"

	"github.com/warewulf/warewulf/internal/pkg/wwlog"
)

// HandleGrub handles GRUB configuration requests
func HandleGrub(w http.ResponseWriter, req *http.Request) {
	ctx, err := initHandleRequest(w, req)
	if err != nil {
		return // response already written
	}

	if !ctx.remoteNode.Valid() {
		wwlog.Error("%s (unknown/unconfigured node)", ctx.rinfo.hwaddr)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// Pin the complete node and asset generation before rendering the
	// GRUB descriptor so every later request of this boot resolves only
	// to the recorded files.
	gen, pinErr := pinBootGeneration(ctx.rinfo, ctx.remoteNode, ctx.conf.Warewulf.AutobuildOverlays())
	if pinErr != nil {
		wwlog.ErrorExc(pinErr, "could not pin boot generation")
		sendBootRetry(w, ctx.rinfo, "grub", pinErr)
		return
	}
	ctx.bootgen = gen

	stageFile := path.Join(ctx.conf.Paths.Sysconfdir, "warewulf/grub/grub.cfg.ww")
	tmplData := buildTemplateVars(ctx.conf, ctx.rinfo, ctx.remoteNode, ctx.bootgen)
	sendResponse(w, req, stageFile, tmplData, ctx)
}
