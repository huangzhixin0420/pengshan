# 蓬山 / Pengshan

> 蓬山此去无多路，青鸟殷勤为探看。

蓬山是 [青鸟（Qingniao）](https://github.com/huangzhixin0420/qingniao) iOS 客户端的配套守护进程：安装在你运行 [Hermes Agent](https://github.com/NousResearch/hermes-agent) 的电脑上，为青鸟提供安全接入——设备配对、本地 `hermes serve` 桥接、与 relay 的端到端加密通道。

**Pengshan is the companion daemon for the Qingniao iOS client. Installed on the machine running your Hermes Agent, it provides secure access for the app: device pairing, local bridging to `hermes serve`, and an end-to-end-encrypted channel through the relay.**

## 状态

早期筹备中（Phase 1 动工）。设计原则：

- **E2E 加密**：relay 只转发密文，服务端不接触对话明文
- **零网络配置**：出站反连，无需公网 IP / 端口映射
- **最小依赖**：基于官方 `hermes serve` JSON-RPC 面，不侵入 hermes-agent 本体

## License

MIT
