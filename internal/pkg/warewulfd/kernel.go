package warewulfd

import (
	"net/http"

	"github.com/warewulf/warewulf/internal/pkg/kernel"
	"github.com/warewulf/warewulf/internal/pkg/wwlog"
)

// HandleKernel handles kernel binary requests
func HandleKernel(w http.ResponseWriter, req *http.Request) {
	ctx, err := initHandleRequest(w, req)
	if err != nil {
		return // response already written
	}

	var stageFile string

	if !ctx.remoteNode.Valid() {
		wwlog.Error("%s (unknown/unconfigured node)", ctx.rinfo.hwaddr)
	} else if ctx.bootgen != nil {
		// Pinned boot: resolve only the generation's protected kernel.
		stageFile, err = ctx.bootgen.resolveStage("kernel", ctx.rinfo.compress)
		if err != nil {
			sendGenerationGone(w, ctx, err)
			return
		}
	} else {
		kernel_ := kernel.FromNode(&ctx.remoteNode)
		if kernel_ == nil {
			wwlog.Error("No kernel found for node %s", ctx.remoteNode.Id())
		} else {
			stageFile = kernel_.FullPath()
			if stageFile == "" {
				wwlog.Error("No kernel path found for node %s", ctx.remoteNode.Id())
			}
		}
	}

	sendResponse(w, req, stageFile, nil, ctx)
}
