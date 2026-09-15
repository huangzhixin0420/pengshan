//go:build race

package relayctl_test

import "time"

// race 模式加密/转发开销 ~4x：放宽窗口、跳过大帧性能用例。
const echoCaseTimeout = 120 * time.Second

const bigSize = 0
