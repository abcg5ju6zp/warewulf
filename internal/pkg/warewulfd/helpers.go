package warewulfd

import (
	"bytes"
	"fmt"
	"net/http"
	"net/netip"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	warewulfconf "github.com/warewulf/warewulf/internal/pkg/config"
	"github.com/warewulf/warewulf/internal/pkg/kernel"
	"github.com/warewulf/warewulf/internal/pkg/node"
	"github.com/warewulf/warewulf/internal/pkg/util"
	"github.com/warewulf/warewulf/internal/pkg/wwlog"
)

// buildTemplateVars constructs the templateVars struct with all necessary
// fields, including handling IPv6 authority formatting and kernel version resolution.
func buildTemplateVars(conf *warewulfconf.WarewulfYaml, rinfo parsedRequest, remoteNode node.Node, gen *bootGeneration) *templateVars {
	kernelArgs := ""
	kernelVersion := ""
	if remoteNode.Kernel != nil {
		kernelArgs = strings.Join(remoteNode.Kernel.Args, " ")
		kernelVersion = remoteNode.Kernel.Version
	}
	if kernelVersion == "" {
		if kernel_ := kernel.FromNode(&remoteNode); kernel_ != nil {
			kernelVersion = kernel_.Version()
		}
	}

	authority := fmt.Sprintf("%s:%d", conf.Ipaddr, conf.Warewulf.Port)
	ipaddr6 := ""
	if confIpaddr6, err := netip.ParseAddr(conf.Ipaddr6); err == nil {
		ipaddr6 = confIpaddr6.String()
	}
	if rinfoIpaddr, err := netip.ParseAddr(rinfo.ipaddr); err == nil {
		if rinfoIpaddr.Is6() {
			if ipaddr6 != "" {
				authority = fmt.Sprintf("[%s]:%d", ipaddr6, conf.Warewulf.Port)
			} else {
				wwlog.Error("No valid IPv6 address configured, but request is IPv6")
			}
		}
	} else {
		wwlog.Error("Could not parse request IP address: %s", rinfo.ipaddr)
	}

	bootID := ""
	if gen != nil {
		bootID = gen.data.ID
	}

	return &templateVars{
		Id:            remoteNode.Id(),
		Cluster:       remoteNode.ClusterName,
		Fqdn:          remoteNode.Id(),
		Ipaddr:        conf.Ipaddr,
		Ipaddr6:       ipaddr6,
		Port:          strconv.Itoa(conf.Warewulf.Port),
		TLS:           conf.Warewulf.TLSEnabled(),
		Authority:     authority,
		Hostname:      remoteNode.Id(),
		Hwaddr:        rinfo.hwaddr,
		ImageName:     remoteNode.ImageName,
		Ipxe:          remoteNode.Ipxe,
		KernelArgs:    kernelArgs,
		KernelVersion: kernelVersion,
		BootID:        bootID,
		Root:          remoteNode.Root,
		NetDevs:       remoteNode.NetDevs,
		Tags:          remoteNode.Tags}
}

// Descriptor served when a generation cannot be pinned because the complete
// asset set does not exist. It explicitly restarts the node instead of
// emitting URLs that could resolve to a mix of current files.
const ipxeBootRetryTemplate = `#!ipxe
echo !!
echo !! %s
echo !! Boot assets are incomplete; restarting the boot in 15s...
echo !!
sleep 15
reboot
`

const grubBootRetryTemplate = `echo "!!"
echo "!! %s"
echo "!! Boot assets are incomplete; restarting the boot in 15s..."
echo "!!"
sleep 15
reboot
`

// sendBootRetry serves a minimal descriptor that directs the node to restart
// its boot because no complete generation could be pinned. format is either
// "ipxe" or "grub".
func sendBootRetry(w http.ResponseWriter, rinfo parsedRequest, format string, cause error) {
	body := ""
	switch format {
	case "grub":
		body = fmt.Sprintf(grubBootRetryTemplate, cause.Error())
	default:
		body = fmt.Sprintf(ipxeBootRetryTemplate, cause.Error())
	}
	w.Header().Set("Content-Type", "text")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write([]byte(body))
	nodeID := ""
	if rinfo.hwaddr != "" {
		nodeID = rinfo.hwaddr
	}
	updateStatus(nodeID, rinfo.stage, "BOOT_RETRY", rinfo.ipaddr)
}

// sendResponse handles the common response logic for provision handlers.
// If tmplData is non-nil, it renders the stageFile as a template. Otherwise, it
// sends stageFile as a raw file (with optional .gz compression).
//
// When the request is bound to a boot generation, only that generation's
// protected files are ever served and a successful send is recorded as
// delivered; a missing or changed file yields 410 Gone so the node restarts
// rather than silently falling back to current assets.
func sendResponse(w http.ResponseWriter, req *http.Request, stageFile string, tmplData *templateVars, ctx *requestContext) {
	wwlog.Serv("stage_file '%s'", stageFile)

	if util.IsFile(stageFile) {

		if tmplData != nil {
			if ctx.rinfo.compress != "" {
				wwlog.Error("Unsupported %s compressed version for file: %s",
					ctx.rinfo.compress, stageFile)
				w.WriteHeader(http.StatusNotFound)
				return
			}

			// Create a template with the Sprig functions.
			tmpl := template.New(filepath.Base(stageFile)).Funcs(sprig.TxtFuncMap())

			// Parse the template.
			parsedTmpl, err := tmpl.ParseFiles(stageFile)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				wwlog.ErrorExc(err, "")
				return
			}

			// template engine writes file to buffer in case rendering fails
			var buf bytes.Buffer

			err = parsedTmpl.Execute(&buf, tmplData)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				wwlog.ErrorExc(err, "")
				return
			}

			w.Header().Set("Content-Type", "text")
			w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
			_, err = buf.WriteTo(w)
			if err != nil {
				wwlog.ErrorExc(err, "")
			}

			wwlog.Info("send %s -> %s", stageFile, ctx.remoteNode.Id())

		} else {
			if ctx.rinfo.compress == "gz" {
				stageFile += ".gz"

				if !util.IsFile(stageFile) {
					if ctx.bootgen != nil {
						sendGenerationGone(w, ctx, fmt.Errorf("compressed file missing from boot generation: %s", stageFile))
						return
					}
					wwlog.Error("unprepared for compressed version of file %s",
						stageFile)
					w.WriteHeader(http.StatusNotFound)
					return
				}
			} else if ctx.rinfo.compress != "" {
				wwlog.Error("unsupported %s compressed version of file %s",
					ctx.rinfo.compress, stageFile)
				w.WriteHeader(http.StatusNotFound)
			}

			// For a pinned boot the selected protected file was verified
			// by the stage handler; re-check it here to close the window
			// between resolution and transfer.
			if ctx.bootgen != nil {
				if err := ctx.bootgen.verifyProtectedFile(stageFile); err != nil {
					_ = ctx.bootgen.invalidate()
					sendGenerationGone(w, ctx, err)
					return
				}
			}

			err := sendFile(w, req, stageFile, ctx.remoteNode.Id())
			if err != nil {
				wwlog.ErrorExc(err, "")
				return
			}

			if ctx.bootgen != nil {
				if err := ctx.bootgen.markDelivered(ctx.rinfo.stage); err != nil {
					wwlog.Warn("could not record delivery of %s in boot generation %s: %s",
						ctx.rinfo.stage, ctx.bootgen.data.ID, err)
				}
			}
		}

		updateStatus(ctx.remoteNode.Id(), ctx.rinfo.stage, path.Base(stageFile), ctx.rinfo.ipaddr)

	} else if stageFile == "" {
		w.WriteHeader(http.StatusBadRequest)
		wwlog.Error("No resource selected")
		updateStatus(ctx.remoteNode.Id(), ctx.rinfo.stage, "BAD_REQUEST", ctx.rinfo.ipaddr)

	} else {
		if ctx.bootgen != nil {
			sendGenerationGone(w, ctx, fmt.Errorf("file missing from boot generation: %s", stageFile))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		wwlog.Error("Not found: %s", stageFile)
		updateStatus(ctx.remoteNode.Id(), ctx.rinfo.stage, "NOT_FOUND", ctx.rinfo.ipaddr)
	}
}

// sendGenerationGone writes a 410 Gone response that explicitly directs the
// node to restart its boot. It is the only response when a pinned generation
// can no longer provide its recorded file; no replacement file is served.
func sendGenerationGone(w http.ResponseWriter, ctx *requestContext, err error) {
	wwlog.Error("boot generation %s for node %s: %s",
		ctx.bootgen.data.ID, ctx.bootgen.data.NodeID, err)
	w.WriteHeader(http.StatusGone)
	updateStatus(ctx.bootgen.data.NodeID, ctx.rinfo.stage, "GEN_GONE", ctx.rinfo.ipaddr)
}
