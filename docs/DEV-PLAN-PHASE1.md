# 蓬山 Phase 1 开发方案（供审核）

> 输入：`docs/PLAN-PHASE1.md`（已审架构）+ hermes-link 源码通读
> 本文档把 PLAN 落到：选型（候选/理由/优势）、模块接口、协议帧示例、目录树、测试矩阵、任务拆解。
> 状态：待审。审核通过且 §11 拍板点落定后开工。

## 1. 技术选型总表

| # | 事项 | 选定 | 候选与理由 |
|---|---|---|---|
| 1 | 语言/工具链 | Go 1.24+，禁 cgo | 已定（DECISIONS.md）。分发形态 = `curl \| bash` 单二进制，只有 Go/Rust 合格；Rust 开发速度不匹配 |
| 2 | WS 库 | `coder/websocket`（nhooyr） | daemon/relay/app 三端同一库。**理由**：context 友好（取消传播直通）、活跃维护、API 干净、支持 WASM（未来 web 客户端零成本）。gorilla 维护史颠簸；gobwas 太底层要自管分帧 |
| 3 | SQLite 驱动 | `modernc.org/sqlite` | **理由**：纯 Go 免 cgo → 交叉编译干净、单二进制兑现。mattn/go-sqlite3 要 cgo，交叉编译要 per-target C toolchain，直接违背分发形态 |
| 4 | E2E 加密 | stdlib `crypto/ecdh`(X25519) + `golang.org/x/crypto/{chacha20poly1305,hkdf}` | 无第三方加密库。**不用 Noise 框架的理由**：3-DH 握手 ~150 行自写，全链路可读可审（E2E 卖点要求"任何人可验证"，依赖越少审计面越小）；代价是自己写对——用测试向量 + fuzz 兜底（§9）。**ChaCha20-Poly1305 而非 AES-GCM**：无 AES-NI 设备上软件实现更快，Go x/crypto 一等支持 |
| 5 | 终端 QR | `skip2/go-qrcode` | 生成矩阵 → half-block ASCII 输出，终端二维码唯一需求，零依赖 |
| 6 | CLI 框架 | `spf13/cobra` | 子命令/flag/help/补全白拿，i18n 挂法成熟。依赖体积对单二进制无感；stdlib flag + 自写分发省不了多少还要自己补 help |
| 7 | 日志 | stdlib `log/slog`（JSON）+ 自写 ~40 行 rotate | 不引第三方日志库；`pengshan logs` 读自家 JSONL，格式自掌控 |
| 8 | 密钥存储 | `~/.pengshan/identity.key`（0600） | 不上 macOS Keychain：Go 免 cgo 就不能碰 Keychain，Phase 1 不上 cgo。威胁模型自洽：能读 identity.key 的进程本来就能读 serve 的 token/.env，同一信任级 |
| 9 | relay 形态 | 独立 main（`cmd/pengshan-relay`）共享 internal/ 帧层 | 与 daemon 同仓同发版；部署 = scp 单文件 + systemd unit |
| 10 | 更新通道 | GH Release + `checksums.txt`（sha256） | `pengshan update` 拉对应 GOOS/GOARCH 资产、校验、替换、自重启；签名（cosign/minisign）留 M5 视威胁模型加 |

**总依赖**（go.mod 预期）：`coder/websocket`、`modernc.org/sqlite`、`skip2/go-qrcode`、`spf13/cobra`、`golang.org/x/crypto`。五个，再无其他。

## 2. 协议细化（相对 PLAN 的两处修正 + 帧定义）

### 2.1 隧道 = E2E 加密字节流（修正 PLAN §4.4 的"WS 消息透传"表述）

app 的认证前奏（`POST /auth/password-login` → `POST /api/auth/ws-ticket`）是 HTTP 请求，必须过隧道。因此隧道抽象为**一条可靠有序字节管道**（虚拟 TCP）：

```
app 侧：HTTP client 与 WS client 都指向隧道本地端点
  │  明文 HTTP/1.1 请求字节 ──encrypt──▶ tunnel ──decrypt──▶ daemon
daemon 侧：反向代理到 http://127.0.0.1:<serve_port>
  │  /api/ws?ticket=… 升级 101 之后 = 纯字节直通，不做任何协议解析
```

- 隧道之上 WS 消息分帧由 app 与 serve 两端协议栈各自处理，蓬山中间只搬字节，**零协议知识**。
- 附件/流式/补拉全走升级后的 WS 字节流，自然兼容。
- daemon 侧实现 = `net.Dial("tcp", 127.0.0.1:port)` + 双向 `io.Copy`，桥接模块不解析 JSON-RPC。
- 优势：最小实现、协议漂移免疫（serve 协议升级与蓬山无关）、审计面最小。

### 2.2 信令/数据双连接（修正 PLAN §4.3 的单 control socket 多路复用）

PLAN 沿用了 hermes-link 的单 control socket + 请求复用。hermes-link 可行是因为它的 payload 是 HTTP 请求/响应小消息；蓬山隧道里是流式 token（高频小帧）+ 附件 base64（大帧）+ 补拉（突发大帧），与心跳/拓扑信令抢一条 socket 会 head-of-line 阻塞。

**改为**：

- **control**：daemon → relay 一条出站 WSS。帧：`open {channel_id}` / `close {channel_id}` / `topology {...}` / `ping/pong`。小、低频、有锁串行写。
- **data**：每条隧道两条 socket——daemon 收到 `open` 后主动回连 `WSS /attach/{channel_id}`，app 连接 `WSS /tunnel/{channel_id}`，relay 把两者对接成双向字节流。
- **能力凭证 = channel_id 本身**：128bit 随机、daemon 生成、`open` 帧下发、TTL 60s（app 须在此窗口 attach，逾期 relay 清映射）、attach 成功即消费（不可复用）。relay 不验任何 token、不持密钥——无秘密性由"猜不到 + 短期 + 一次性"保证，砍掉 hermes-link 的 connect token/JWT 体系（少一个密钥生命周期、少一类 bug）。
- app 怎么拿到 channel_id：app 连 relay 前先与 daemon 有加密信令通道？不——更简单：**app 的 attach 请求里带配对身份**：app WSS 连 `/tunnel`，首帧（隧道握手前、明文）发 `{device_id, client_hello}`，relay 按 device 路由到 daemon 的 control 连接上作为 `attach-request` 事件；daemon 验设备公钥握手 → 建 channel → `open` 回 relay → relay 对接。即 channel_id 甚至不出 daemon 的控制面。流程见 §2.4。

### 2.3 隧道握手（3-DH）

双方材料：设备长期密钥对（app）、daemon 长期密钥对（daemon，公钥经 QR 给 app）。每隧道：

```
app ──WSS──▶ relay ──WSS──▶ daemon        （/tunnel，首帧明文信封）
  信封 = {device_id, e_dev(临时公钥), 签名(device长期私钥, transcript_prefix)}

daemon 验：签名（有设备公钥）→ 生成 e_psn → 混合 3-DH：
  s1 = ECDH(dev_long,  psn_long)   配对时已知常量
  s2 = ECDH(e_dev,     psn_long)
  s3 = ECDH(dev_long,  e_psn)      ← app 需用设备私钥解 e_psn（信封回包）
  key = HKDF-SHA256(s1‖s2‖s3, salt=transcript_hash, info="pengshan-tunnel-v1")
  → {tx_key, rx_key}（两端方向相反）

daemon ──▶ app  回包信封（明文）：{e_psn, 签名(psn_long, transcript)}
app 验签名（QR 里的 daemon 公钥）→ 同式派生 key。
此后全部 data 帧密文；n 单调计数 = nonce；窗口外/重复 n 即拒。
```

- 前向安全：e_dev/e_psn 每连接一次，泄露长期私钥不解历史流量。
- 中间人：transcript = 两端交换的全部信封字节序列的 SHA-256，签名绑死。
- 优势自评：~150 行、stdlib+x/crypto、可逐行审计；不用 Noise 是为了审计面（§1-#4），但**格式按 Noise_XX 的精神对齐**（静态+临时混合、transcript 绑定）。

### 2.4 数据帧格式

隧道外（app↔relay↔daemon，data socket）：

```json
{ "v": 1, "n": 42, "t": "data", "b": "<base64 XChaCha20-Poly1305 ciphertext>" }
{ "v": 1, "n": 43, "t": "ping", "b": "" }
{ "v": 1, "n": 44, "t": "close", "b": "<可选 reason ciphertext>" }
```

- `n`：每方向独立 uint64，从握手 transcript 末值起始，**跳变 >1024 即断**（防重放也防位翻转）。
- 加密用 XChaCha20-Poly1305（24 字节 nonce：`n` 8 字节 + 固定 16 字节零填充），避免短 nonce 拼接工程。
- control socket 帧（不加密，relay 可见）：`open/close/topology/ping/pong/attach-request/attach-ready`，均为 JSON。

### 2.5 relay 对接状态机

```
app          relay                    daemon
 │ /tunnel ──▶ │ attach-request ──────▶ │ control
 │             │  （按 device 路由）      │ 验签名 → 派生 key → open {ch}
 │             │ ◀──── open {ch} ─────── │
 │ ◀─ 101 ─────│ /tunnel 挂起待配对        │
 │ 握手信封 ──▶ │ 原样转 ─────────────────▶ │ 验签 → 回包
 │ ◀────────── │ ◀─ 回包信封 ──────────── │
 │ 密文 data ─▶ │ ◀─ daemon /attach/{ch} ─ │ （daemon 主动回连）
 │ ◀──────────▶ │ 双向对接（纯转发）         │ ◀──────────▶
```

- relay 内存态：`{device_id → control_conn}`、`{ch → {app_conn|nil, daemon_conn|nil, exp}}`，两端正向即对接、任一端断开即清。
- relay 绝不落盘（无状态除瞬时映射）——重启即全清，daemon 重连重建。

## 3. 目录树与模块接口

```
pengshan/
  cmd/pengshan/main.go           CLI 入口（cobra）
  cmd/pengshan-relay/main.go     relay 入口
  internal/
    version/version.go           版本/构建信息（-ldflags 注入）
    config/config.go             ~/.pengshan/config.json；XDG 路径
    identity/identity.go         长期密钥对：LoadOrCreate(path) (PublicKey, signer)
    e2e/
      handshake.go               3-DH 握手：Initiator/Responder, transcript
      frame.go                   data 帧 seal/open、n 校验
      keys.go                    HKDF 派生
    pairing/
      manager.go                 CreateSession/Claim/Revoke/List
      qr.go                      QR 载荷构造
      envelope.go                配对提交信封的验签/构造
    storage/
      storage.go                 Open(path)、Migrate()
      devices.go / sessions.go / audit.go
    relayclient/
      control.go                 control 连接生命周期（bootstrap/refresh/心跳/重连退避）
      attach.go                  处理 attach-request → 开 channel → 回连 /attach
      topology.go                拓扑上报快照
    tunnel/
      endpoint.go                隧道字节端点：net.Conn 兼容接口（Read/Write 帧化）
      relayconn.go               relay data socket 的读写循环
    bridge/
      proxy.go                   反向代理：隧道端点 ↔ 127.0.0.1:<serve_port> TCP
      probe.go                   serve 健康探测（GET /api/health）
    relayctl/                    （relay 服务端）
      server.go                  HTTP+WS 服务（/tunnel /attach /control /pairing）
      registry.go                device→control、channel 映射
      pairingbox.go              配对信封中转
      tokens.go                  bootstrap/refresh/access token（HS256，TTL 与 hermes-link 同款）
    cli/                         pair/devices/unpair/start/stop/status/logs/doctor/update/autostart/version
    daemon/
      daemon.go                  组装：identity+storage+relayclient+bridge+probe
      autostart.go               launchd/systemd 安装卸载
      update.go                  GH Release 自更新
      logs.go                    slog JSONL + rotate
    i18n/i18n.go                 zh-CN/en 文案表
  scripts/install.sh
  scripts/pengshan-relay.service
  scripts/cn.hzxprzp.pengshan.plist
```

关键接口签名（审核重点）：

```go
// internal/e2e
type HandshakeResult struct { SendKey, RecvKey []byte /* 32B */ ; TranscriptHash [32]byte }
func Respond(serverLong, serverEphem crypto.PrivateKey, clientHello []byte, devPub []byte) (*HandshakeResult, []byte, error)
func Initiate(clientLong, clientEphem crypto.PrivateKey, serverPub, hello, response []byte) (*HandshakeResult, error)

// internal/tunnel
type Endpoint struct{ /* net.Conn 兼容；内部按 64KB 切块成 data 帧 */ }
func NewEndpoint(ws *websocket.Conn, hr *e2e.HandshakeResult, dir Direction) *Endpoint

// internal/bridge
func ServeProxy(tun net.Conn, serveAddr string, probe *probe.Prober) error // io.Copy 双向 + WS 升级直通

// internal/relayctl
type Server struct{ /* http.Server + registry */ }
func (s *Server) handleTunnel(w, r)   // attach-request → 路由 → 101 → 对接
func (s *Server) handleControl(w, r)  // daemon control 注册 + 帧循环
```

## 4. 存储模型（SQLite，四表）

```sql
devices(device_id PK, label, platform, pub_key BLOB, paired_at, last_seen, revoked_at NULL)
pairing_sessions(session_id PK, code, expires_at, scopes, claimed_by NULL, created_at)
audit_events(id PK AUTO, at, actor_type, actor_id, event, detail_json)
meta(k PK, v)   -- schema_version, connect_signing_secret 等
```

token 不落盘为明文（无 bearer token 概念——app↔daemon 是设备密钥挑战，无 daemon 侧 access token；relay 的 refresh/access token daemon 侧要存，sha256 存储照 hermes-link）。

## 5. CLI 命令面（与 PLAN §5 一致，补实现要点）

- `pair`：建 pairing_session → 渲染 QR（payload `{v,kind:"pengshan-pair",psn_id,psn_pk,session_id,code,exp,relay_urls[]}`）→ 前台等 claim（10min）→ 成功后打设备摘要。
- `devices list/revoke`；`unpair` 全清重配（二次确认）。
- `start`（前台跑 = `daemon.Run`，debug 用）/`stop` 通过 PID 文件 + signal；`restart`。
- `status`：identity 指纹、配对设备数、relay 连接态、serve 探测结果、本机 LAN 地址。
- `logs -f -n N --level X`：读 `~/.pengshan/logs/*.jsonl`。
- `doctor`：二进制版本、identity 存在性、serve 可达、relay 连通、自启配置存在、磁盘/权限。
- `update`：GH Release latest → 匹配资产 → sha256 校验 → 替换 → 自重启（launchd RunAtLoad 语义见 §7）。
- `autostart on/off`：写 launchd plist（Mac）/ systemd unit（Linux）。

## 6. 自启与更新语义（launchd/systemd）

- launchd：`RunAtLoad=true`、`KeepAlive=false`。语义 = 登录即起、崩溃不自动拉、用户 `stop` 后保持停。daemon 自更新 = 替换二进制 + `launchctl kickstart -k`（新进程起，RunAtLoad 语义不变）。**不采用 KeepAlive=true**：用户明确 stop 后不想再被拉起（与 npm 版 link 同语义）。
- systemd：`[Install] WantedBy=default.target`，`Restart=no`，同语义。
- 更新回滚：新二进制启动自检失败（identity 损坏/配置非法）→ exit 1 + 错误日志；不做自动回滚（M5 视实测再定）。

## 7. 与青鸟的对接面（app 侧改造清单，供 qingniao 排期）

1. `GatewayClient` 增加 tunnel transport：HTTP 前奏（login/ticket）经隧道字节流发出；WS 同。
2. 地址择优扩三地址：LAN / cloudflared 隧道 / pengshan relay 隧道；relay 地址从配对 QR 的 `relay_urls` + 配对响应获取。
3. 配对 UI：扫码（相机）+ 手动输码兜底；设备私钥存 iOS Keychain。
4. 隧道断线语义对齐现有重连逻辑（connect 风暴防护那套直接复用）。
5. 认证策略跟随拍板点 P1：A 透传 = 现有密码登录流原样跑在隧道里，**app 零协议改动**，只换 transport。

app 侧工作量估计：transport + 配对 UI 约 600–900 行 Swift（估）。

## 8. 里程碑任务拆解（PR 粒度）

**M1 骨架**（目标：本机 echo 往返）
- 1.1 go.mod + 目录树 + version/config 基建
- 1.2 `internal/e2e`：握手 + 帧层 + 测试向量（固定私钥对，断言 key/transcript，逐字节断言两帧）
- 1.3 `internal/relayctl`：registry + /tunnel /attach /control 对接（明文 echo 先跑通）
- 1.4 `internal/tunnel.Endpoint`：net.Conn 兼容 + 分块/重组
- 1.5 集成脚本：relay+daemon(mock bridge echo)+客户端三进程本机跑通
- 验收：`go test ./...` + 集成脚本 64KB/1MB/64MB 随机字节往返无损

**M2 配对**
- 2.1 identity LoadOrCreate + 指纹
- 2.2 storage 四表 + Migrate
- 2.3 pairing manager + QR 渲染 + claim 验签 + 吊销
- 2.4 relay 配对信封中转（pairingbox）
- 2.5 CLI：pair/devices/unpair
- 验收：CLI 手工全流程（pair → claim → devices list → revoke → 隧道拒连）

**M3 桥接（全链）**
- 3.1 relayclient：bootstrap/refresh token + control 生命周期 + attach 处理
- 3.2 daemon 组装 + bridge 反向代理 + probe
- 3.3 mock app（Go 客户端：握手 + HTTP login/ticket + WS 流式）
- 3.4 真 serve 端到端：9120 开发实例 login→ticket→session.list→prompt→message.delta→events.since
- 3.5 模拟器 app 全链（三地址择优接入）
- 验收：§3.4/3.5 全绿 + 附件（image.attach_bytes 1MB）+ 弱网重连（30% 丢包 tc）补拉正确

**M4 分发**
- 4.1 install.sh + release workflow（goreleaser：darwin-arm64/amd64、linux-amd64/arm64，checksums）
- 4.2 autostart launchd/systemd + doctor 扩展
- 4.3 update 命令
- 4.4 干净机器演练（无 Go 环境 Mac：install → pair → doctor 全绿）
- 验收：4.4 通过 + 更新后版本号与进程存活核验

**M5 打磨**
- 5.1 topology 上报 + relay 快照接口
- 5.2 重连风暴压测（100 并发 attach/open 循环）+ 退避参数实测调优
- 5.3 fuzz（frame/握手解析）+ go vet/staticcheck
- 5.4 README 用法 + PROTOCOL.md（协议草案公开，供第三方审计）
- 5.5 可选：cosign 签名、Keychain 存储调研（仅出结论不实现）

## 9. 测试矩阵

| 层 | 内容 | 工具 |
|---|---|---|
| 单元 | 握手向量（固定 X25519 私钥 → 断言 transcript/hash/key）、帧 seal/open 往返、n 重放/跳跃拒绝、HKDF 长度/上下文 | `go test` |
| 单元 | storage CRUD、pairing 过期/重放 claim/并发 claim、吊销后隧道拒 | `go test` + 临时 SQLite |
| 模糊 | frame 解析、握手信封解析（随机字节不 panic、不泄漏 key 材料） | `go test -fuzz` 各 5min |
| 集成 | relay+daemon+mock serve；echo 完整性；WS 升级直通；中途断连 | 脚本 + `go test -tags=integration` |
| 端到端 | 真 serve：login→ticket→list→prompt→流式→补拉→附件 1MB→弱网重连 | 3.4/3.5 |
| 压测 | 单隧道吞吐/延迟基线；100 设备并发 attach；control 心跳 CPU | `go test -bench` + k6（仅 relay 转发面） |
| 验收口径 | 同 qingniao 惯例：证据目录 + 复跑脚本 + 负向对照（吊销设备必拒） | scratch/ |

加密代码评审要求：握手/帧层 PR 必须附测试向量文件（`internal/e2e/testdata/*.json`）供第三方复算。

## 10. 风险与对策

| 风险 | 对策 |
|---|---|
| 自写 3-DH 写错 | 固定向量测试 + fuzz + transcript 绑定 + 评审时逐行对 Noise_XX 精神核对；PROTOCOL.md 公开接受审计 |
| WS 当字节流的边界语义（message 分帧丢失） | 隧道帧自带长度分块（64KB），不依赖 WS message 边界；集成测试跨 64KB 边界用例 |
| relay 单点（Phase 1 单节点） | QR 载荷 `relay_urls` 数组支持多 relay；daemon config 同；txy 挂 = 远程断，LAN 直连不受影响 |
| channel_id 被猜/被复用 | 128bit 随机 + 60s TTL + attach 即消费 + relay 绑定两端地址不对第三方可见 |
| serve 端口漂移 | config 显式优先 + probe 失败给明确错误；doctor 输出修复建议 |
| GH Release 被投毒 | checksums 校验 + （M5 可选）cosign；install.sh 固定版本号不追 latest |

## 11. 待拍板（开工前）

- **P1 认证**：PLAN 推荐 A（透传）不变；本方案按 A 设计（app transport 换、协议不动）。若改 B（daemon 代铸），bridge 前要加 ticket 代铸层，M3 顺延 2–3 天。
- **P2 relay 归属**：推荐 monorepo（§1-#9 已按此设计）。
- **P3 开发节点**：本机 + txy。
- **P4 域名**：`get.hzxprzp.cn` 或 GH raw（install.sh 主载体是 GitHub，域名仅展示）。

## 12. 与 PLAN-PHASE1 的差异小结（本方案相对输入的修正）

1. §2.1：隧道从"WS 消息透传"修正为"E2E 加密字节流 + 反向代理"——覆盖 HTTP 认证前奏。
2. §2.2：control/data 分连接，砍 connect token，channel_id 即能力凭证——解决流式 head-of-line，减密钥生命周期。
3. 其余章节（模块、CLI、存储、里程碑）为 PLAN 的细化展开，无方向性变更。
