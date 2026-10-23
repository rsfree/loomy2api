# 逆向协议参考（Loomy / 讯飞）

本文档描述本项目所依赖的上游协议形态，供互操作与学习使用。
内容来自对官方 Loomy 桌面客户端（Electron `0.9.38`）行为的观察与公开参考实现，
**与讯飞无任何关联**。

> 本仓库只描述**协议结构**（端点、鉴权形状、请求/响应字段）。
> 客户端内置的签名常量在 `internal/config` 里作为可覆盖的默认值存在，
> 可在 `config.json` 的 `access_key_id` / `access_key_secret` 中替换为你自己客户端的值。

---

## 1. 组件与端点

| 组件 | Base URL | 用途 |
|---|---|---|
| 模型网关 | `https://loomyad.xunfei.cn/api/v1` | OpenAI 兼容模型访问（`/chat/completions`、`/models`、`/embeddings`、`/images/generations`、`/search/tencent`、`/points/records`） |
| 账号服务 | `https://account.xfinfr.com` | 登录、签发 session、用户信息 |
| 交易服务 | `https://trade.xfinfr.com` | 支付（本项目不用） |
| Nexus 网关 | `wss://dispatch-nexus.xfinfr.com/ws/loomy` | 远控长连（本项目不用） |
| 官网 / 微信回调 | `https://loomy.xunfei.cn` | 微信扫码回调域 |

## 2. 鉴权模型

模型网关用 **登录态本身** 作为 Bearer token，不发放独立 API key：

```
Authorization: Bearer <session>
token: <session>                      # 双写，上游两种拼写都接受
traceparent: 00-<32 hex>-<16 hex>-01  # 必需；缺失会让上游挂死到超时
loomy-version: 0.9.38                 # 仅做存在性检查
Content-Type: application/json
```

可选的归属头（影响服务端积分记账）：`ChatId`、`MsgId`、`TurnId`、
`X-Loomy-Request-Purpose`（取值如 `chat.message`、`chat.title`、`kb.embed`、
`image.generate`、`search.web` 等）。

- **双写 `Authorization` 与 `token`**：客户端 provider 配置里是 `useSessionAuth: true`，
  网关两种拼写都认，所以本项目两个都发。
- **`traceparent` 不能省**：实测缺失时请求会挂到超时而不是快速报错，
  这是最容易踩的坑。本项目每个请求都重新生成。

### 登录态生命周期

- 有效期 **14 天**（客户端登录时显式申请 `expire=1209600`）；
- **没有 refresh token**，过期只能重新登录；
- 因为密码登录可全自动，所以「存手机号 + 密码」是无人值守的前提。

## 3. 账号服务签名

请求头 `Authorization` 的形态是 `account <accessKeyId>:<signature>`，
签名算法为 **HMAC-SHA1**。

`stringToSign` 固定 **9 行**（无 `x-*` 头时最后两行为空串，但**不能省略**）：

```
METHOD                                   # 大写，如 POST
ESCAPED_PATH                             # 逐段 RFC3986，额外转义 !'()*
ESCAPED_QUERY                            # 无 query 时为空串
Content-MD5                              # base64(md5(body))，无 body 时为空串
Content-Type                             # application/json
Date                                     # 如 Sat, 26 Sep 2026 12:00:00 GMT
Nonce                                    # 一个 UUID
SignedHeaders                            # 排序后的 x-* 小写名，用 ; 连接
CanonicalizedHeaders                     # 排序的 "k:v\n" 拼接，去掉末尾换行
```

```
signature     = base64(HMAC-SHA1(accessKeySecret, stringToSign))
Authorization = "account <accessKeyId>:<signature>"
```

实现见 `internal/account/signer.go`，单测 `signer_test.go` 覆盖了行数、转义、
自定义头规范化与签名稳定性。

## 4. 登录流程

### 4.1 密码登录（全自动）

```
POST /login/account/getPuKey   → { pukey: <base64 DER 公钥>, rcode: <服务端 nonce> }
POST /login/account/byPwd      → { session, userid, phone }
   body: { loginid, password: RSA_PKCS1v15(pukey, 明文密码), rcode, type: 1, expire: 1209600 }
```

关键点：**`rcode` 是服务端下发的 nonce，不是验证码**，所以密码登录完全可自动化，
不需要人工看图或输码。

公钥是 DER 编码（PKIX `SubjectPublicKeyInfo`）；本项目同时兼容裸 PKCS#1，
并容忍 base64 缺失 padding（Node 的 `Buffer.from` 容忍，Go 的标准库不容忍）。

### 4.2 短信登录（需人工读一次码）

```
POST /login/phone/sendMsgCode  → { msgid }
POST /login/phone/checkCode    → { session }
```

### 4.3 其他

```
POST /userinfo/query/baseInfo   # 校验登录态是否仍有效
POST /login/account/logout
```

### 4.4 请求信封

账号服务的每个请求体都是同一个信封：

```json
{
  "base": {
    "appid": "GM3LOOMY",
    "modelid": "Web",
    "version": "1.0.0",
    "devid": "web",
    "ua": "Loomy|Desktop|Electron|macOS",
    "traceid": "<每请求新生成的 32 hex>"
  },
  "param": { "...": "各端点自己的参数" }
}
```

**`devid` 必须是固定常量 `web`**。所有官方客户端（含 Windows 版）都发这个值，
自创格式（如 `web-<hex>`）在全部真实流量里都是异常信号。设备维度区分由
promotions 侧的 `campus_device_id` 承担（客户端每台机器一个），本项目按账号保持独立。

## 5. 模型网关

### 5.1 请求

```http
POST {base}/chat/completions
body: { model, messages, temperature, max_tokens, stream,
        enable_thinking, chat_template_kwargs: { enable_thinking } }
```

- 模型标识形态为 `provider/model`，如 `imodel/spark-x`。
- 推理模型会在 `message.reasoning_content` 里给思考链；
  可用 `enable_thinking: false`、`chat_template_kwargs.enable_thinking`
  或消息前缀 `/no_think` 抑制。

### 5.2 响应

- 非流式：`choices[].message.reasoning_content` + `content`；
  `usage` 含 `completion_tokens_details.reasoning_tokens`（思考 token）、
  `points_consumed`（本次扣的积分）、`prompt_tokens_details.cached_tokens`。
- 流式：标准 OpenAI SSE（`data: {...}`），末尾 `data: [DONE]`。

### 5.2.1 模型目录（`GET /models`，2026-10-21 抓包核对）

真实响应结构（13 个模型实测）：

```json
{"reasoning_enabled": true, "object": "list", "data": [{
  "id": "DeepSeek-V4.1-Flash",
  "name": "DeepSeek V4.1 Flash(x2.0)",
  "type": "chat", "protocol": "openai_chat",
  "context_length": 1048576, "max_output_tokens": 384000,
  "is_default_model": true, "default_reasoning_effort": "low",
  "reasoning_efforts": ["none","low","medium","high","xhigh"],
  "capabilities": {"reasoning": true, "vision": true, "function_calling": true,
                   "streaming": true, "system_message": true,
                   "input_modalities": ["text","image"], "output_modalities": ["text"]}
}]}
```

几条必须注意的口径：

1. **没有 `multiplier` 字段**。倍率写在 `name` 的括号里：`(x2.0)`、`（x4.0）`
   全角括号也出现过。网关用 `[（(]\s*[xX×]\s*([0-9.]+)\s*[）)]` 解析，缺失则留空。
2. **「限时免费」同样只在 `name` 里**：`Spark-X2.6-preview(限时免费)(x0.0)`。
   判定条件是「名称含 限时免费/限免」或「解析出的倍率为 0」。网关据此置
   `is_free`，面板在倍率列渲染「限免」标签、统计限免数，并对 `/v1/models`
   额外输出 `is_free` / `multiplier: 0`（OpenAI 兼容客户端可忽略）。
3. `capabilities` 是**对象**（布尔键 + `input_modalities` 数组），不是数组；
   `type=chat|image` 决定是否图像模型；`auto` 是路由型模型，无倍率。
4. **认不出的模型名不会报错，而是静默回落默认模型**（2026-10-21 实测）：
   `{"model":"no-such-model-xyz"}` 返回 **HTTP 200**，而响应体里的 `model` 变成
   `DeepSeek-V4.1-Flash` —— 客户端拿到一个"成功"的错答案，积分按默认模型扣。
   这与「未知请求字段静默忽略」是同一套上游行为。**网关因此必须自己做目录校验**
   （`strict_model_check`，默认开）：不在 `/models` 里的模型直接 404，不打上游；
   响应 `model` 与请求不一致时另记一条告警，作为第二道证据。

5. **图像模型（`type=image`）只服务 `/images/generations`**：对 `/chat/completions`
   上游统一回 `404 该模型暂未开放，请切换其他模型`（与「模型名写错」不可区分）。
   网关因此把它们从对话路径上摘掉——`/v1/models` 对这类条目输出
   `endpoint: "/v1/images/generations"` 与 `supports_chat: false`，三个对话端点
   （`chat_completions` / `responses` / `messages`）在本地直接 400 指路，不打上游。
   而**上游 404 本身也不再罚账号**：它记进模型级失效台账（见下），请求直接 404、
   不打上游，30 天 TTL 自愈。

6. **404 ≠ 账号故障**：`该模型暂未开放` 这类 404 只说明这个**模型**不可用，换号、
   等冷却都不会好。旧实现按 `FaultNotFound` 罚账号 60 秒，一个打错模型名的请求就能
   把池里所有号依次冻掉。现在改为：记模型台账 + 从 `/v1/models` 隐藏 + 不重试不罚号。

### 5.3 额度与积分

```
availableBalance = balance（长期积分）+ dailyBalance（每日赠送剩余）
```

模型有倍率（`x0.1` … `x12.0`），决定一次请求扣多少积分；`spark-x` 在 `x0.1` 最便宜。

#### 5.3.1 真实响应结构（2026-10-21 抓包核对）

两个端点的职责不同，**必须都打**：

```
GET  {base}/points/records?record_type=all&pageNo=1&pageSize=100
POST {base}/points/first-login          # body: {}
```

`points/records` 只回三个余额字段：

```json
{
  "code": "000000", "desc": "成功", "trace_id": "…",
  "data": {
    "balance": 5000, "dailyBalance": 5000, "availableBalance": 10000,
    "pageNo": 1, "pageSize": 1, "total": 1,
    "list": [{
      "ledgerId": "pl_b55bd6d28242485f8382e9bcbd52934e",
      "modelName": "", "direction": "credit", "description": "注册奖励",
      "createdAt": 1792389369, "pointsActual": 5000, "consumeSource": "",
      "balanceBefore": 5000, "balanceAfter": 10000, "dailyCycleDate": "2026-10-19"
    }]
  }
}
```

`dailyQuota` / `dailyConsumed` / `permanentBalance` **只有 `first-login` 才返回**：

```json
{
  "code": "000000", "desc": "成功",
  "data": {
    "alreadyProcessed": true,
    "currentBalance": 10000, "permanentBalance": 5000, "dailyBalance": 5000,
    "dailyQuota": 5000, "dailyConsumed": 0, "dailyCycleDate": "2026-10-21",
    "registerReward": 0, "inviteeReward": 0, "inviterReward": 0,
    "invitationCodes": [{"inviteCode": "FWF6D5", "usedCount": 0, "maxUses": 1000, "status": "active"}]
  }
}
```

> `dailyCycleDate` 与 `list[].dailyCycleDate` 是两个不同的日期：
> 前者是当前每日额度所属日（今日），后者是该笔流水发生时所属日。

#### 5.3.2 额度口径（上游明确）

- **每日赠送额度登录后刷新为 5000**；客户端每次启动都会调一次 `points/first-login`，
  该端点是幂等的（当天已领过返回 `alreadyProcessed: true`，不再重复加）。
- **扣费顺序：先扣活动 / 兑换码来源的积分，再扣普通长期积分**（`permanentBalance`）。

因此本项目把额度拆成三块展示，不再做单值折算：

| 字段 | 来源 | 语义 |
|---|---|---|
| `balance` | records.balance | 长期积分余额 |
| `dailyBalance` | records.dailyBalance | 每日赠送剩余 |
| `availableBalance` | records.availableBalance | 可用总额 |
| `permanentBalance` | first-login | 长期积分（口径同上，交叉校验） |
| `dailyQuota` | first-login | 每日赠送上限（5000） |
| `dailyConsumed` | first-login | 当日已消耗 |
| `dailyCycleDate` | first-login | 每日额度所属日 |

实现见 `internal/upstream.Quota`；单测 `TestQuotaDailyFromFirstLogin`
用真实响应样本守着「records 拿不到每日额度、必须走 first-login」这条事实。

**签到（领取每日积分）的网关侧适配**：Loomy 没有独立"签到"按钮端点——
官方客户端每次启动都会调一次 `POST points/first-login` 来刷新每日赠送额度，
这就是 WorkBuddy 式"模拟登录/签到/领积分"动作在本协议里的等价实现。
网关把它封装为 `upstream.Checkin`（解析 `alreadyProcessed` / 各额度字段 /
`invitationCodes`，并直接产出 `Quota` 快照），面板提供：

| 端点 | 行为 |
|---|---|
| `POST /panel/api/accounts/{name}/checkin` | 单号签到（幂等，已领过返回 already_processed=true） |
| `POST /panel/api/checkin_all` | 全池签到（跳过禁用与登录态失效账号） |
| 登录成功（单号登录 / 新增带密码账号） | 自动链式签到 + 刷额度 |
| 定时额度刷新（quota_refresh_minutes） | Quota 内部顺带调 first-login，跨日自动续领 |

单测 `TestCheckinParsesRealSample` / `TestCheckinAccount` / `TestCheckinAll`
用抓包样本守着这条链路。

#### 5.3.3 两个已踩过的坑

**坑一：上游只认 camelCase 的 `pageSize`**。传 `page_size` 会被静默忽略并回落默认 20 条，
不会报错——排查起来极其费时。本项目在 `internal/upstream` 用常量固定了拼写，
并有单测 `TestQuotaCamelCasePageSize` 守着。

**坑二：成功码是 `"000000"` 而不是 `"0"`**。上游 `HTTP 200` 的 body 里
`code: "000000"` 表示成功，`desc: "成功"`。早期的错误信封判定只认 `"0"` / `"200"`，
把每一个成功的积分响应都改判成 502，导致额度刷新永久失败——而 `points/records`
本身是好的，问题出在本地判定。单测 `TestErrEnvelopeSuccessCodes` 覆盖了各成功码形态。

#### 5.3.4 每日额度的重置时刻 = 自然日 00:00（2026-10-21 实测）

`dailyBalance` 按**自然日**重置（0 点后即可继续用），不是「撞墙时刻 + 24h」，也不是
本地 04:00。额度耗尽的硬冷却终点因此取次日 00:00（`pool.NextDailyReset`）——
按 04:00 会让 23:50 烧空额度的账号白等 4 小时。

⚠️ 这个口径不可从别家实现互抄：WorkBuddy 的每日周期是 04:00，两家结算周期不同。

#### 5.3.5 新手任务（`/onboarding/*`，合计 10000 积分）

```
GET  {base}/onboarding/tasks            → data.tasks{8 key: bool}, data.earned, data.total
POST {base}/onboarding/tasks/complete   body {"key":"..."} → data{alreadyCompleted, balance}
```

- **服务端不校验前置行为**（2026-10-21 实测）：直接 POST 即可拿分，不需要真的发对话 /
  生成 PPT / 安装技能 —— 完成条件全在客户端本地判定，服务端只做「幂等置位 + 加分」。
- 8 项与分值：`first_message` 500 / `pick_skill` 1000 / `generate_ppt` 1500 /
  `set_schedule` 1000 / `install_skill` 1500 / `configure_remote` 1000 /
  `create_soul` 1500 / `share_soul` 2000（合计 10000）。
- **幂等判据是响应体的 `alreadyCompleted`**，不是 HTTP 码、不是信封里的 `code`。
- 响应里的 `earned` 是**累计已领**而非本次增量，网关一律按本地表现算（官方客户端同做法）。
- 领取后 `balance` 会变，需要再打一次 `points/records` 才能拿到新的 `dailyBalance`。

### 5.4 长任务与非流式的 504

上游对「长时间无响应的**非流式**请求」约 60 秒就返回 504；而**同一模型的流式调用**
能跑几分钟。所以本项目对非流式长任务（`max_tokens >= keepalive_min_tokens`）
内部改用 `stream: true` 调上游，边收 SSE 边聚合，最后拼回非流式 JSON——
客户端完全无感，长任务不再被掐断。

### 5.5 生图 / 图生图（`/images/generations`，2026-10-23 客户端代码核对）

官方客户端的图片工具（`electron/image/image-generation-service.js`）只有两条路径，都打
`POST {base}/images/generations`，请求头带 `X-Loomy-Request-Purpose: image.generate`：

```json
// 文生图
{"model":"doubao-seedream-5-lite","prompt":"一只橘猫","n":1,
 "size":"2304x1728","response_format":"url"}

// 图生图（1..N 张参考图，官方客户端上限 4）
{"model":"doubao-seedream-5-lite","prompt":"把第一张图的产品放进第二张图的场景",
 "images":["data:image/png;base64,iVBORw0...","https://example.com/room.jpg"],
 "n":1,"size":"2304x1728","response_format":"url"}
```

几条必须注意的口径：

1. **参考图字段是复数 `images`（数组）**，单图也发数组——客户端不分单双，一律发数组，
   且从不上报单数 `image`。单数 `image` 只认字符串：传数组会被**静默忽略**，
   出图与原图无关（2026-10-22 实测，各扣 110 分）。
2. 数组元素可以是 URL 或 `data:image/...;base64,`，**两者可混用**；顺序即提示词里的
   「第一张 / 第二张」，客户端按附件顺序 map，不乱序。
3. 默认参数 `n=1`、`size=2304x1728`、`response_format=url` 由客户端写死（`_generateTextToImage`
   与 `_generateImageToImage` 共用），`size` 另接受 `2K` / `4K` 预设。
4. 响应是 OpenAI 形态：`data[].url`（或 `b64_json`）；**扣分在顶层 `points_consumed`**，
   没有 `usage`。计费按张，三个图像模型 × 任意分辨率统一 110 积分/张，耗时 16–48s。
5. **4 张上限来自前端**（main bundle 常量 `Cu=4`，超出提示「最多上传 4 张图片」），
   不是服务端报错——所以 >4 的服务端行为未知；网关按同口径截断，不送未经验证的张数。
6. 网关对**客户端侧**的字段收口（`image` 字符串/数组、`null`/`[]` 视为没图、空串 400、
   单张 32M 字符上限、`mask` 依赖 `image`）对齐参考项目
   `image-adapter` 的 `validate_images_body`，细节见 README「生图：参考图字段的真实形态」。

## 6. 风险控制相关的观察

- 客户端身份字段（`devid` / `ua` / `modelid` / `version`）是**常量**，
  改这些不会降低风控判定，反而更显眼。
- 真正决定风险画像的是网络出口；本项目提供全局 `proxy` 与账号级 `proxy`。
- 请求归属头（`ChatId` / `MsgId` / `TurnId`）影响积分记账口径，建议透传。

## 7. 已知局限

- 没有 refresh token → 登录态到期只能重登，因此需要保存手机号 + 密码。
- `/v1/messages` 是本地翻译层（上游只有 OpenAI 形态），工具调用与思考链已覆盖，
  但 Anthropic 的一些细节（如 `cache_control` 断点）无法传递到上游。
- 模型目录以 `/models` 为准；上游不可用时回落到内置表（`internal/config`）。
- 多图参考：字段形态已与官方客户端对齐（`images` 数组），但「多张图是否都被模型真正读进去」
  还没像单图那样做 dhash/NCC 取证（单图实测 dhash≤1、NCC≥0.997）。要拍板只能实跑一次：
  两张互不相似的垫图 + 一句依赖两张图的提示词，每次 110 积分。

---

本项目仅供学习研究，使用者需自行遵守上游服务条款。
