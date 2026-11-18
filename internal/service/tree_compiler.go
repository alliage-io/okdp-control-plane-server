package service

import (
	compiler "github.com/okdp/okdp-compiler"
	"github.com/okdp/okdp-gitops/catalog"
	"github.com/okdp/okdp-gitops/gitops"
)

// NewTreeCompiler is the compile of the server's writes and reads
// (gitops.Deployments.Apply, Tree): the catalogs of catalogs, the charts
// of charts, and no render of the charts (gitops.Compiler.SkipRender).
// Helm's engine takes no context, so a render past its timeout would leave
// its goroutine in this long-running process, and its memory is bounded
// only once it returns; the CI's compile of each commit renders, the
// boundary. A write that only the render refuses is committed, and the
// CI's compile refuses the instance.
func NewTreeCompiler(catalogs *catalog.Loader, charts compiler.ChartSource) *gitops.Compiler {
	c := gitops.NewCompiler(catalogs)
	c.Charts = charts
	c.SkipRender = true
	return c
}
