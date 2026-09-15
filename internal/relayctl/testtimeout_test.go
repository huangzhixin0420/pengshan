//go:build !race

package relayctl_test

import "time"

// echoCaseTimeout 是单个子用例的完成窗口（race 模式见 race 构建标签文件）。
const echoCaseTimeout = 30 * time.Second

// bigSize 是大帧用例（64MB）；race 模式置 0 跳过（race 下 seal 开销 ~4x，
// 大帧用例是性能测试，并发正确性由 64KB/1MB 在 race 下覆盖）。
const bigSize = 64 << 20
