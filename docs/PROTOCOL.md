# 蓬山隧道协议（PROTOCOL.md v1）

> 本文档公开供第三方审计与独立实现。加密实现见 `internal/e2e/`，
> 复算向量见 `internal/e2e/testdata/handshake-vector.json`。
> 发现安全问题请开 issue 或邮件维护者。

## 1. 威胁模型与目标

**目标**：iOS 青鸟 ↔ 用户 Mac 上 hermes serve 的端到端加密通道，经不可信 relay 转发。

**防**：
- relay 运营者被动/主动窥探（只见密文与元数据：时间、流量大小、设备 ID）
- 回放/篡改注入（帧级 AEAD + 单调序号）
- 设备丢失（配对可吊销，吊销即时断连）

**不防（Phase 1 已知限制）**：
- 流量时序/体积分析
- daemon 所在 Mac 被完全控制（它本来就有 serve 的全部权限）
- relay 拒绝服务（可用性靠多 relay 回落，QR 载荷 `relay_urls` 数组）

## 2. 密钥学

### 2.1 长期身份密钥

- daemon：X25519 私钥，`~/.pengshan/identity.key`（0600），首次启动生成
- app：X25519 密钥对（设备密钥），私钥 iOS Keychain，公钥配对时提交 daemon

### 2.2 会话握手（每隧道一次）

双方各生成临时 X25519 密钥对 `e_dev` / `e_psn`。app 发 hello，daemon 回 respond：

```
hello   = {v:1, t:"hello", device_id, e_dev: b64u(32B), ts: unix秒}
respond = {v:1, t:"respond", e_psn: b64u(32B), ts: unix秒}
```

新鲜性：双方校验对方 `ts` 在 ±300s 窗口内。序列化规则：UTF-8 JSON、
字段按本文档顺序、紧凑分隔符（Go `json.Marshal` struct 序、无空白）。

3-DH 混合派生：

```
s1 = X25519(dev_long,  psn_long)     配对时经认证通道交换长期公钥
s2 = X25519(e_dev,     psn_long)
s3 = X25519(dev_long,  e_psn)
ikm  = s1 ‖ s2 ‖ s3                   （按此固定顺序拼接，96 字节）
salt = SHA256(hello_bytes ‖ respond_bytes)     transcript 绑定
okm  = HKDF-SHA256(ikm, salt, info="pengshan-tunnel-v1", 64 字节)
app 方向：tx = okm[0:32]，rx = okm[32:64]
daemon 方向相反
```

**认证性**：s1/s3 需设备长期私钥，s2 需 daemon 长期私钥；公钥交换走
已认证通道（QR 含 daemon 公钥、配对 claim 含设备公钥）。中间人没有
任一方长期私钥即派生不出 key，首帧解密失败即断开（隐式认证）。
**前向安全**：临时密钥每连接一次，泄露长期私钥不解历史流量。
**防重放**：整包重放因每次新临时密钥无法完成派生；部分重放被序号治理拒绝。

**独立复算**：`internal/e2e/testdata/handshake-vector.json` 给出固定种子
私钥与期望 hello/respond/方向密钥。任何实现用同种子与固定时钟
（ts=1700000000）应得到逐字节一致的结果（重新生成：
`PENGSHAN_UPDATE_VECTOR=1 go test ./internal/e2e/`）。

## 3. 隧道帧（data socket，双向同构）

```json
{ "v": 1, "n": 42, "t": "data", "b": "<base64url(XChaCha20-Poly1305 ciphertext)>" }
```

| 字段 | 语义 |
|---|---|
| `v` | 协议版本，恒 1 |
| `n` | 每方向单调递增 uint64，从 1 起；**严格 +1 校验**，重放/跳帧即断开 |
| `t` | `data`（密文）/ `close`（密文 reason）/ `err`（密文）/ `ping` / `pong`（明文空体） |
| `b` | 密文帧：AEAD 输出 base64url；ping/pong 恒空 |

加密：XChaCha20-Poly1305，nonce = `n` 的 8 字节大端 ‖ 16 字节零（24B）。
单帧明文上限 64KB；上层字节流按 64KB 分块。AAD 未用（帧头篡改导致
nonce/序号不符，解密自败）。

## 4. 配对协议

```
daemon: 生成 pairing_session {session_id, code(8位 Crockford), exp=+10min}
QR JSON = {v:1, kind:"pengshan-pair", claim_urls[], psn_id, psn_pk, session_id, code, exp}
app:    POST <claim_url> {session_id, code, device_id, label, platform, pub_key}
daemon: 原子验码（未过期/未 claim）→ 注册设备（pub_key 32B X25519）→ 返回 {ok, device_id, psn_id}
```

- code：Crockford Base32 8 位（无 0/O/1/I），手输容错（小写/连字符/空白）
- 会话并发上限 4；claim 原子（SQL 层防并发双 claim）
- 吊销：`devices revoke` 删公钥 → 该设备后续 attach 在握手前被拒
- relay 中转配对（远程无 LAN）预留：claim_urls 可指 relay 地址

## 5. relay 拓扑（无秘密转发器）

### 5.1 端点

| 端点 | 方向 | 语义 |
|---|---|---|
| `GET /control` | daemon→relay | control 长连（注册/心跳/信令） |
| `GET /tunnel` | app→relay | 开隧道：relay 生成 attach_id 经 control 发 `attach-request`，挂起等回连 |
| `GET /attach/{id}` | daemon→relay | daemon 回连，两端齐 → 双向 message pump（直通） |
| `GET /healthz` | 任意 | 存活 |
| `GET /api/v1/topology` | 任意 | daemon 最新快照（LAN 地址/serve 状态/版本/设备数，app 择优用） |

### 5.2 control 帧（relay 可见，不加密）

```json
{ "v": 1, "t": "<type>", "p": { } }
```

| t | 方向 | 载荷 |
|---|---|---|
| `register` | daemon→relay | `{role:"daemon", id}` |
| `ping` / `pong` | 双向 | 空 |
| `attach-request` | relay→daemon | `{attach_id}` |
| `attach-deny` | daemon→relay | `{attach_id, reason}`（设备拒/内部错） |
| `topology` | daemon→relay | 见 5.1 topology 端点 |

WS 层 ping 25s/timeout 20s；daemon 重连退避 3→60s。
attach 窗口默认 15s（超时清理）；数据面与信令面分连接，避免流式
head-of-line。

### 5.3 对接语义

两端 attach 即 pump 启动（`sync.Once` 防双泵）；**任一方向 EOF/出错即
两端 CloseNow**（不能等双方向都退——静默方向会架空清理）。pump 逐
message 转发，保留 WS message 边界。

## 6. 桥接（daemon 本地）

隧道字节流 = 一条虚拟 TCP：daemon `net.Dial(serve)` + 双向 `io.Copy`。
HTTP/1.1 前奏（认证）与 WS 升级（101 后）全直通，零协议解析。
serve 不可达 → 拒绝 attach（`serve_unavailable`）。

## 7. 配套

- 握手向量：`internal/e2e/testdata/handshake-vector.json`（固定种子可复算）
- 帧/握手 fuzz：`go test ./internal/e2e/ -fuzz FuzzOpen` / `-fuzz FuzzHandshakeEnvelope`
- 风暴压测：`go test ./internal/relayctl/ -run TestRelayStorm -v`（100 并发 ×3 轮全生命周期）
- e2e 冒烟：`scratch/mockapp`（隧道内 HTTP+WS 全链演示客户端）

## 8. Roadmap（安全相关候选，按优先级）

| 项 | 结论 | 理由 |
|---|---|---|
| cosign/sigstore 签名 | **首个正式版（非 rc）前接入**：GH Actions 内签名，update/install 验签 | 当前 sha256 只防传输损坏；签名防 release 账号/仓库被盗后的资产投毒 |
| daemon 私钥进 macOS Keychain | Phase 1.5 候选：Go 免 cgo 实现不了原生 Keychain，需 `security` CLI 子进程桥 | 收益（私钥不出文件系统）vs 复杂度（子进程桥 + 首次授权弹窗）不划算于 Phase 1；威胁模型里 Mac 被完全控制=已失守 |
| 多 daemon 路由（relay 读 hello 按 device 路由） | Phase 1.5+：当前单 daemon 取唯一 control | 需求出现时加 device→control 路由表 |
| 流量填充（防时序/体积分析） | 不做：成本（带宽×N）与体验（省电）权衡不划算 | 会话内已是密文，元数据保护非 Phase 1 目标 |

