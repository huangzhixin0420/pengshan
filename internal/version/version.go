// Package version 承载构建注入的版本信息。
// 发布流水线通过 -ldflags "-X github.com/huangzhixin0420/pengshan/internal/version.Version=vX.Y.Z" 注入。
package version

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// UserAgent 供 daemon 在 relay bootstrap / 拓扑上报时标识自己。
func UserAgent() string { return "pengshan/" + Version }
