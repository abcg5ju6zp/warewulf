package warewulfd

import (
	"net/http"
	"path"

	"github.com/warewulf/warewulf/internal/pkg/wwlog"
)

// HandleIpxe handles iPXE boot script requests
func HandleIpxe(w http.ResponseWriter, req *http.Request) {
	ctx, err := initHandleRequest(w, req)
	if err != nil {
		return // response already written
	}

	var stageFile string
	var tmplData *templateVars

	if !ctx.remoteNode.Valid() {
		wwlog.Error("%s (unknown/unconfigured node)", ctx.rinfo.hwaddr)
		stageFile = path.Join(ctx.conf.Paths.Sysconfdir, "/warewulf/ipxe/unconfigured.ipxe")
		tmplData = &templateVars{
			Hwaddr: ctx.rinfo.hwaddr}
	} else {
		// Pin the complete node and asset generation for this boot before
		// emitting any stage URLs. A new boot started later may use a new
		// generation, but this boot will resolve only to these files.
		gen, pinErr := pinBootGeneration(ctx.rinfo, ctx.remoteNode, ctx.conf.Warewulf.AutobuildOverlays())
		if pinErr != nil {
			wwlog.ErrorExc(pinErr, "could not pin boot generation")
			sendBootRetry(w, ctx.rinfo, "ipxe", pinErr)
			return
		}
		ctx.bootgen = gen
		template := ctx.remoteNode.Ipxe
		if template == "" {
			template = "default"
		}
		stageFile = path.Join(ctx.conf.Paths.Sysconfdir, "warewulf/ipxe", template+".ipxe")
		tmplData = buildTemplateVars(ctx.conf, ctx.rinfo, ctx.remoteNode, ctx.bootgen)
	}

	sendResponse(w, req, stageFile, tmplData, ctx)
}
