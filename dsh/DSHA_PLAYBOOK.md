# DSHA 真机实战手册（给后来的 AI）

> **用途**：在安卓真机（DSHA = DeepSeek Harness Android Launcher）上打通容器 shell、安装/调试插件、与局域网 Hub 通信。
> **读法**：直接跳到第 0 节抄命令。若出问题，按第 9 节决策树走。
> **价值**：本文记录的所有路径均已实测通过。按本文执行可跳过约 60+ 次无效探索（`adb root` / `run-as` / token 认证 / HTTP 直链 / TS 源码发布等都是死路）。
>
> **本文件位置**：手机 `/sdcard/DSHA_PLAYBOOK.md`（PC 侧副本，仓库根 `D:/AI-Agent/vibe/Agent-core-contect`：`dsh/DSHA_PLAYBOOK.md`）。
> 读取：`adb -s <target> shell cat /sdcard/DSHA_PLAYBOOK.md`
> 若担心 /sdcard 被清理，可在容器终端持久化一份：
> `cp /sdcard/DSHA_PLAYBOOK.md /root/local-plugins/DSHA_PLAYBOOK.md`

---

## 0. TL;DR —— 最快路径（复制即用）

```bash
# 环境（Windows Git Bash，必须先设）
export MSYS_NO_PATHCONV=1          # 防止 /sdcard 被转成 C:/Program Files/Git/sdcard
ADB="D:/AI-Agent/dsh-phone/platform-tools/adb.exe"
T="192.0.2.202:43829"            # 手机无线调试地址（端口每次重开都会变）

# 1) 连手机（有多台设备时后续所有命令必须带 -s "$T"）
"$ADB" connect $T

# 2) 拿到容器 root shell 的执行权（核心套路）
#    把脚本 push 到 /sdcard  →  在 DSHA 终端里执行  →  输出写回 /sdcard  →  读回
"$ADB" -s "$T" push your.sh /sdcard/your.sh
"$ADB" -s "$T" shell input tap 600 1400                    # 点终端主体，获取焦点
"$ADB" -s "$T" shell "/system/bin/input text 'sh'"         # 注意：必须全路径调用
"$ADB" -s "$T" shell input keyevent 62                     # 空格（input text 不支持空格！）
"$ADB" -s "$T" shell "/system/bin/input text '/sdcard/your.sh'"
"$ADB" -s "$T" shell input keyevent 66                     # 回车
sleep 5
"$ADB" -s "$T" shell "cat /sdcard/your.log"                # 读回输出
```

三行的本质：**容器文件 adb 看不见，但容器把 `/sdcard` 挂进来了**。所有交互都走「文件共享 + 终端注入」。

---

## 1. 环境事实表（已验证，可直接采信）

| 项 | 值 |
|---|---|
| 手机 | 荣耀 SDY-AN00，Android 14（SDK 34） |
| DSHA 包名 / 版本 | `com.dsh.client` / App 0.2.21 |
| 容器内 dsh 版本 | **0.2.0-rc.2** |
| 容器内 Node | **v24.19.0** |
| 容器 | PRoot + Ubuntu，容器内 uid=0(root)，**手机本身无 root** |
| DSH_HOME / profile | `/root/.dsh` / `web` |
| 容器暴露端口 | 3080(dsh web)、3081(LanProxy)、3090 |
| 容器↔Android 共享目录 | `/sdcard` |
| 手机无线调试 | 192.0.2.202:43829（**每次重开无线调试端口都会变**） |
| 电脑 Hub | 192.0.2.78:8080(TCP) / 8081(HTTP)，监听 `0.0.0.0` |

**容器内可直接用的命令**：`dsh`、`node`、`npm`、`pnpm`、`sh`、`nc`、apt 源可用（能联网装包）。

---

## 2. ADB 无线调试通道

```bash
export MSYS_NO_PATHCONV=1          # 必设！否则 /sdcard 路径被 Git Bash 转换
ADB="D:/AI-Agent/dsh-phone/platform-tools/adb.exe"
T="192.0.2.202:43829"

"$ADB" connect $T
"$ADB" devices -l
```

**必须注意**

- **实测 `adb devices` 会有 3 台**（物理设备 USB、无线调试、mDNS 发现项各一条）：
  ```
  ERLDU19613005705                                  device
  192.0.2.202:43829                               device
  adb-AJTYUT2630003756-fwfAYu (2)._adb-tls-connect._tcp  device
  ```
  之后**每条命令都要 `-s "$T"`**，否则报 `more than one device/host`。
- 手机侧 AD 提示：设置 → 开发者选项 → 无线调试 → 使用配对码配对（首次），之后可直接 `connect ip:port`。
- 端口每次重开无线调试都会变，**先用 `adb devices` 确认，别硬编 43829**。
- 端口转发（需要时）：`"$ADB" -s "$T" forward tcp:3080 tcp:3080`

---

## 3. 容器 shell 通路（本文最关键的突破）

### 3.1 为什么普通方法都走不通（**别再试了**）

| 尝试 | 结果 |
|---|---|
| `adb root` | ❌ `adbd cannot run as root in production builds` |
| `run-as com.dsh.client` | ❌ App 非 debuggable，不可用 |
| `adb shell ls /data/user/0/com.dsh.client` | ❌ 权限 700 |
| 容器 3080/3081 API + token | ❌ 401（详见第 7 节） |
| `adb shell dsh ...` | ❌ adb shell 只到 Android 的 shell 用户，dsh 在 PRoot 容器里 |

### 3.2 真正的通路：DSHA 内置的 PTY 终端

DSHA 底部导航第 4 个是「终端」页，标题为 **Ubuntu · PTY** —— 它是容器内的真实 root shell。

**进入方法**

```bash
"$ADB" -s "$T" shell input tap 1050 2548     # 底部导航 → 终端页
sleep 3
"$ADB" -s "$T" shell input tap 1020 602      # 点「+」新建终端（重要，见下）
sleep 5
```

**为什么必须新建终端**：DSH 服务每次重启，旧终端会话会失效（输入无反应且**无任何报错**）。症状是"命令敲了但文件没生成"——此时点 (1020,602) 新建即可，不要去排查别的。

### 3.3 输入注入的三条铁律

1. **必须直接调 `/system/bin/input`**
   写 `adb shell "input text 'date>/sdcard/a.txt'"` 时，`>` 会被 adb 的 `sh -c` 当成重定向吃掉，命令变成写入 adb 端文件。
   ✅ 正确：`"$ADB" -s "$T" shell "/system/bin/input text 'date>/sdcard/a.txt'"`

2. **`input text` 不支持空格**
   空格会被丢弃或截断。用 `input keyevent 62`（KEYCODE_SPACE）拼接。

3. **复杂命令一律写成 .sh 文件**
   逐字注入既慢又易错。把命令写进脚本 push 到 /sdcard，终端里只敲 `sh /sdcard/x.sh`。

### 3.4 结果回读

**uiautomator 抓不到终端内容**（终端是自定义绘制，不是 TextView）。所以：

```bash
# 脚本内
{
  echo "=== 结果 ==="
  your_command
} > /sdcard/wbxxx.log 2>&1

# PC 侧
"$ADB" -s "$T" shell "cat /sdcard/wbxxx.log"
```

**模板脚本**（所有容器脚本都套这个骨架）：

```sh
#!/bin/sh
export DSH_HOME=/root/.dsh
OUT=/sdcard/your.log
{
  echo "=== whoami ==="
  id -un
  echo "=== 你的命令 ==="
  ...
} > $OUT 2>&1
```

---

## 4. 文件进出容器

```bash
# 推入（Windows 路径要用 D:/... 形式）
"$ADB" -s "$T" push "D:/path/to/file" /sdcard/file

# 拉取（例如导出编译产物）
"$ADB" -s "$T" pull /sdcard/out.js "D:/path/to/dist/out.js"
```

容器内路径视角：`/sdcard` 即容器里的 `/sdcard`（bind mount），可直接读写。

> 注意：不要长期把插件依赖放在 /sdcard 上（会被系统媒体扫描、也可能被清理）。最终依赖应落到容器内部路径，如 `/root/local-plugins/`。

---

## 5. UI 自动化（DSHA 四个页面）

底部导航（**坐标随分辨率而变，见 5.1**）：

| 位置 | 页面 |
|---|---|
| 第 1 个 | 启动（状态 / 重启 / 停止 / 启动 DSHA） |
| 第 2 个 | 插件（含「从链接安装」+ 插件管理 tab） |
| 第 3 个 | 设置 |
| 第 4 个 | 终端 |

### 5.1 坐标必须 dump 获取，不要猜

**这是本次最大的时间陷阱。** 我曾按"猜"的坐标点「重启」，结果全部落空且**界面毫无反应**，导致误判成"重启功能不生效"，浪费大量轮次。真正坐标是 y≈886，猜的是 y≈455。

```bash
# 一键拿到当前页所有可点击节点 + 坐标
"$ADB" -s "$T" shell "uiautomator dump /sdcard/ui.xml" >/dev/null 2>&1
"$ADB" -s "$T" shell "grep -oE '<node[^>]*clickable=\"true\"[^>]*>' /sdcard/ui.xml" \
  | sed -E 's/.*text="([^"]*)".*bounds="([^"]*)".*/\1 | \2/'
# bounds 形如 [600,886][1128,1102]，取中心点：x=(600+1128)/2, y=(886+1102)/2
```

参考（1200×2640 逻辑分辨率实测值，机型不同必须重取）：

```
底部导航 4 个按钮：150 / 450 / 750 / 1050  ，y=2548
启动页：  重启 (349,886)   停止 (851,886)   启动 DSHA (600,710)
插件页：  插件管理 tab (864,465)   只看自己装的 开关 (600,1057)   直链输入框 (600,1204)
终端页：  新建终端 (1020,602)   终端主体 (600,1400)
```

**另一机型：华为 YAL-AL10（1080×2340，Android 10）** —— `uiautomator dump` 常报
`ERROR: could not get idle state`（启动页日志区持续刷新导致），此时改为 `screencap` 截图目测坐标：

```
底部导航 4 个按钮：x≈130 / 400 / 670 / 944 ，y≈2230
终端主体 (540,1400)   启动 DSHA (540,608)   停止 (765,806)
换算：截图宽度 500px → 实际 1080，系数 2.16；截图高度 1030 → 2340，系数 2.27
```

**又一台机型：某国产 Android 14（SDY-AN00，1200×2652）** —— 2026-10-24 实测，
**底部导航在 y≈1600，不是 2548**（旧记录里的 2548 打过去毫无反应，白排查半小时）：

```
底部导航 4 个按钮：x≈150 / 450 / 750 / 1050 ，y≈1600
启动页：  ▶ 进入工作台/启动 DSHA (600,704)   ↻ 重启 (350,886)   □ 停止 (851,886)
终端页：  ＋ 新建 (1020,602)   终端主体 (600,1400)
```

> **别猜、也别信旧记录**：布局随 DSHA 版本变（手机一 dsh 0.2.0-rc.2 与手机二
> 0.2.1-rc.2 的启动页按钮就差了一截）。可靠做法是 `uiautomator dump` 后**按
> text 取 bounds**，而不是按机型背坐标。dump 出来的 `text` 是屏幕坐标，
> 直接取中心点即可；只有当 dump 的 text 为空（Compose 未生成语义节点）时，
> 才退回截图目测。

**screencap 返回 0 字节时的排查**（2026-10-24 在某国产 Android 14 上遇到）：

```
adb shell screencap -p /sdcard/x.png   →   文件存在但 0 字节
adb exec-out screencap -p > x.png      →   同样 0 字节
dumpsys power | grep mWakefulness      →   mWakefulness=Awake（屏幕没睡）
```

此时**不是息屏、也不是没 root**，而是该机型的 SurfaceFlinger 对当前 adb 会话
拒绝出图。**别在截图上死磕**：直接改用 `uiautomator dump`（同机照常成功），
再配 `dumpsys window | grep mCurrentFocus` 确认前台是谁。
实战里这比截图更可靠——微信/微博等带悬浮窗的应用还会盖住前台窗口，
`am start` 之后必须用 `mCurrentFocus` 复核，必要时先 `input keyevent 3`（HOME）
再 `am start`，否则你以为切到了 DSHA，实际点的是悬浮窗后面的别的应用。

### 5.2 页面文本读取

```bash
"$ADB" -s "$T" shell "uiautomator dump /sdcard/ui.xml" >/dev/null 2>&1
"$ADB" -s "$T" shell "sed 's/></>\n</g' /sdcard/ui.xml | grep -oE 'text=\"[^\"]+\"' | sort -u"
```

---

## 6. 插件安装全流程（可复制）

### 6.1 打包（PC 侧）

```bash
cd D:/AI-Agent/vibe/Agent-core-contect/dsh
npm pack      # → dsh-plugin-lan-hub-0.1.0.tgz
```

### 6.2 推到手机 + 容器内安装

```bash
"$ADB" -s "$T" push dsh-plugin-lan-hub-0.1.0.tgz /sdcard/plugin.tgz
```

容器内脚本 `install.sh`：

```sh
#!/bin/sh
export DSH_HOME=/root/.dsh
{
  # 依赖源放容器内部，别依赖 /sdcard
  mkdir -p /root/local-plugins
  cp /sdcard/plugin.tgz /root/local-plugins/plugin.tgz
  # 覆盖安装前必须删目录 + 升版本号，否则 pnpm 判定已满足会跳过（坑 17）
  rm -rf /root/.dsh/profiles/web/node_modules/dsh-plugin-lan-hub
  cd /root/.dsh/profiles/web
  dsh plugin --profile web add /root/local-plugins/plugin.tgz 2>&1
  # 校验真的换了：grep 一个只有新代码才有的字符串
  grep -c "discovered hub at" /root/.dsh/profiles/web/node_modules/dsh-plugin-lan-hub/dist/index.js
} > /sdcard/install.log 2>&1
```

> `dsh plugin add` 底层就是 pnpm，工作在 `/root/.dsh/profiles/web`，**支持 tarball 本地文件**。

### 6.3 生效：必须「停止 → 启动」，不是「重启」

```bash
# 启动页
"$ADB" -s "$T" shell input tap 851 886     # 停止
sleep 15
"$ADB" -s "$T" shell input tap 600 710     # 启动 DSHA
sleep 90                                    # 完整启动约 60~90s
```

**只点「重启 Web」不会刷新插件清单**，插件不会进入加载流程。

### 6.4 验证

```bash
# PC 侧问 Hub：有没有 phone-agent 上线
node -e "fetch('http://127.0.0.1:8081/api/agents').then(r=>r.text()).then(t=>console.log(t))"
# 期望：{"agents":[{"id":"phone-agent","name":"PhoneAgent"}]}
```

容器内查加载状态：

```sh
{
  echo "=== 本次启动是否加载了本插件 ==="
  grep -iE "lan-hub|failed to import|did not activate" /root/dsh-web.log | tail -20
} > /sdcard/check.log 2>&1
```

---

## 7. 踩坑清单（症状 → 根因 → 解法）

| # | 症状 | 根因 | 解法 |
|---|---|---|---|
| 1 | 点「重启」无反应、状态不变 | **坐标是按猜的，落空了**（最大陷阱） | 先 `uiautomator dump` 取真实 bounds 中心点 |
| 2 | 终端敲命令无反应、文件没生成 | DSH 重启后旧终端会话失效；且**无任何报错** | 点 (1020,602) 新建终端 |
| 3 | 命令里的 `>` 被吃掉 | `adb shell "input ..."` 经 `sh -c`，`>` 被当重定向 | 用全路径 `/system/bin/input text '...'` |
| 4 | 带空格的命令被截断 | `input text` 不支持空格 | 用 `input keyevent 62` 拼空格 |
| 5 | `$IFS` 不展开 | 目标 shell 不是 bash 或注入环境不展开变量 | 直接用 keyevent 62 拼空格，别用 $IFS |
| 6 | 插件 `failed to import`，无堆栈 | **Node 24 的 type stripping 不支持 node_modules 下的文件**；插件正是以 node_modules 包被加载 | 必须编译成 JS：`main` 指向 `dist/index.js` |
| 7 | 插件装不上 / 报版本不满足 | `engines.dsh` 写了 `>=0.2.0`，但 `0.2.0-rc.2 < 0.2.0`（**prerelease 被纯数字范围排除**） | **删掉 `engines.dsh`**，回退到 `peerDependencies: {"@deepseek-ai/dsh": "*"}` |
| 8 | Web 直链安装被拒 | DSHA 只认 **HTTPS** 直链，http 被拒 | 放弃直链，改 `adb push` + 本地 tarball 安装 |
| 9 | 3080/3081 API 一直 401 | UI 上显示的 token 是 **App 侧**凭据，容器 API 要的是另一套，且 DSH 重启会轮换 | 别走 API 路线，走终端 |
| 10 | Hub 上插件已加载但没上线 | 配置里 `host` 是默认的 `127.0.0.1`，在手机上指向**手机自己** | 配置改电脑局域网 IP |
| 11 | 改了配置却不生效 | ①config 是**整块替换不是深合并**，漏字段会退回默认值 ②`patchReload: "startup"` 需重启 | 写全所有字段 + 停止→启动 |
| 12 | 手机连不上电脑端口 | 电脑防火墙 Private/Public profile 为 Enabled | 放行入站；或确认 Hub 监听 `0.0.0.0` 而非 `127.0.0.1` |
| 13 | 改插件配置时误伤 bundle 层 | profile 层 patch 在 bundle 层之后应用 | 用 **id-targeted** 写法：`- update: {id: lan-hub, config: {...}}` |
| 14 | `npm pack` 包内缺文件 | `files` 字段未含 `dist` | `files` 加 `dist`；`test/` 不进包 |
| 15 | 连通性测试永远返回 CONNECT_FAIL，但实际上是通的 | 用了 `(echo > /dev/tcp/...)`——**dash 不支持 `/dev/tcp`**（bash 专有） | 改用 `node -e "net.connect(...)"` 或 `nc -z -w3 ip port` |
| 16 | DSHA 显示 READY，但 Hub 上看不到 phone-agent | 手机息屏 / App 进后台 → 网络被系统冻结，插件连接 `ETIMEDOUT` | **不用干预**：插件指数退避重连会自动恢复。核实：`grep '\[lan-hub\]' /root/dsh-web.log` |
| 17 | 改了插件代码，停止→启动后行为**没变**；`dist/index.js` 的 mtime 也不变 | **版本号没升**（仍旧 0.1.0），`dsh plugin add` 判定「依赖已满足」直接跳过，装的是旧包 | **改版本号**（0.1.0→0.1.1）并 `rm -rf /root/.dsh/profiles/web/node_modules/dsh-plugin-lan-hub` 后再 add；装完必须校验：`grep -c "<新代码里的特征串>" dist/index.js` |
| 18 | 配了 `host: auto`，日志报 `discovery timeout`，最后靠 fallback 连上 | ① 发现超时默认 **2000ms**，本网段 254 个单播扫描来不及（每包还要 ARP）② `fallbackHost` 留空会退回 `127.0.0.1`，在手机上指向手机自己 | 超时提到 **5000ms**、扫描在 400ms 后启动；`fallbackHost` 显式填 Hub 局域网 IP。成功的标志是日志出现 `discovered hub at <ip>:<port> (auto)` |
| 19 | 两台手机都连上 Hub，但只剩一台在线、另一台被反复顶掉 | 两台 `agentId` 相同（都叫 `phone-agent`），Hub 按 id 注册，后到的覆盖先到的 | **一机一个 agentId**（如 `phone-agent` / `huawei-agent`），`agentName` 同步区分 |
| 20 | 偶尔启动时日志出现 `discovery failed: discovery timeout`，但同一个网络下重启一次又好了 | **启动早期**网卡/路由/省电策略的瞬时未就绪，单次发现超时；功能上靠 fallback 仍能连上，但看起来像报错 | 0.1.2 起自动**重试 2 次**（间隔 1.2s）再兜底；失败日志带诊断 `ifaces=N, sent=M, recv=K`：`ifaces=0` 即网卡还没就绪，属瞬时问题，**不必处理** |
| 21 | 日志警告 `dsh: warning: 1 entry did not activate` + `dsh-mesh-fleet: TypeError: invalid mesh-fleet config: expected an object` | mesh-fleet 自带 patch 写的是 `config: ${{config}}`，用户 patch 里没有该条目 → 占位符替换成**字符串**；而 `resolveConfig` 只接受对象/`undefined`/`null` | 在 `cordis.patch.yml` 追加 `- id: dsh-mesh-fleet` / `name: dsh-mesh-fleet` / `config: {}`，重启后应出现 `插件加载完成：dsh-mesh-fleet`。**注意**：它导致 mesh-fleet 的 11 个工具不注册，但**不是**工作台卡死的原因 |
| 22 | **整个 DSHA 界面点哪都没反应**，画面停在最后一帧（还能滚动、内容偶尔还会增加），发消息/切会话都无效 | dsh web 的**本地监听 `127.0.0.1:3080` 被一条挂死的请求堵死**，后续请求全部排队不返回；页面数据全走 3080，所以交互全死。同进程的 `*:3081`（局域网）完全正常，因此**不是进程崩了**。触发点：打开「工作台」页面必然复现（真机两台各复现 2 次） | **区分方法**：`ss -ltn` 看 3080/3081 都在监听 → 设备内 `nc 127.0.0.1 3081` 秒回 401、`nc 127.0.0.1 3080` 一个字节不回 = 就是本条。**恢复**：DSHA「停止 → 启动」（或强杀 App 重开）；重启后只要不点「进入工作台」，3080 长时间稳定 |
| 23 | 插件日志写着「加载完成」、Hub 上 agent 也在线，但**模型一个工具都调不到**；日志里另有 `dsh: warning: 1 entry did not activate` + `JsonSchemaError: ... required must be true when present` | 工具参数写了 JSON Schema 的 **`required: false`**。宿主 `@deepseek-ai/dsh-tools` 的 schema 编译器硬规则：属性上只要带了 `required`，值就必须**严格等于 true**；可选字段要**省略该键**。连接是在 `ctx.effect` 里先建立的，所以连接成功掩盖了注册失败 | 删掉这些 `required: false`（不能改成 `true`，那会让参数变必填）。回归测试：`node --experimental-strip-types test/schema-check.ts`——直接用宿主同款校验器 `assertSupportedJsonSchema` 校验每个工具。**验收信号是「did not activate」消失**，不是「加载完成」 |
| 24 | 配了 `host: auto` 但日志一直 `discovery timeout (ifaces=0, sent=1, recv=0)`，最后靠 fallback 才连上 | **proot 容器里枚举不到可用网卡**（手机一 `ifaces=1` 能发现成功，手机二 `ifaces=0` 全打空）。此时 auto 等于完全失效，全靠 `fallbackHost` 兜底 | `host: 'auto'` **必须同时显式配 `fallbackHost`**（Hub 的局域网 IP）。看日志里是 `discovered hub at`（走通了）还是 `fallback`（兜底），别混淆 |
| 25 | `screencap -p` 生成了文件但**大小 0 字节**，`exec-out` 也一样；`dumpsys power` 显示屏幕 Awake | 该机型 SurfaceFlinger 对当前 adb 会话拒绝出图（**不是息屏、也不是没 root**） | 直接改用 `uiautomator dump`（同机照常成功）取 bounds；别在截图上死磕 |
| 26 | 按旧记录的坐标 tap 底部导航毫无反应，dump 出来的 bounds 又和屏幕坐标对不上 | **布局随 DSHA 版本变**：手机一 dsh 0.2.0-rc.2 与手机二 0.2.1-rc.2 的按钮位置不同；旧机型坐标（2548）对另一台机型无效 | 每台机器现场 `uiautomator dump` 取 bounds 中心点，**不要背坐标**；tap 完再 dump 复核是否切页成功 |
| 27 | `am start` 之后前台仍是别的应用（微博） | 微博的 `WeiboBrowser` 是 **SYSTEM_ALERT_WINDOW 悬浮窗**，优先级高于 Activity，`am start` 抢不回焦点 | `input keyevent 3`（HOME）→ `am start` → 用 `dumpsys window \| grep mCurrentFocus` 复核；必要时 `am force-stop` 那个应用 |
| 28 | 新开一条 shell 后 `adb devices` 空、`adb forward` 也失效 | 每个 Bash 命令是**新会话**，adb server 被回收 → 连接与端口转发一起丢（无线调试设备尤其明显） | 每条命令开头 `adb connect <ip:port>`；**forward 与 curl 必须放在同一条命令里**，否则转发当场失效（曾因此误判成「服务没起来」） |

---

## 8. 网络与连通性

**先单测再装插件**（把网络问题和插件问题分开，否则会误判）：

```sh
# 容器内（用 node 测，别用 /dev/tcp —— 见下方警告）
{
  echo "=== 手机 → 电脑 Hub 连通性 ==="
  node -e "const n=require('net'),s=n.connect(8080,'192.0.2.78',()=>{console.log('CONNECT_OK');s.end()});s.on('error',e=>console.log('CONNECT_FAIL',e.code));s.setTimeout(3000,()=>{console.log('TIMEOUT');s.destroy()})"
} > /sdcard/net.log 2>&1
```

> ⚠️ **不要用 `(echo > /dev/tcp/host/port)` 测连通性**：`/dev/tcp` 是 **bash 专有特性**，
> 而容器脚本的 shebang 是 `#!/bin/sh`（dash），不支持它 → **永远返回 CONNECT_FAIL 的假阴性**。
> 我最初的手册就写错了这条。用 `node -e` 或 `nc -z -w3 <ip> <port>` 才是可靠做法。

PC 侧确认：

```bash
# 监听在 0.0.0.0 才对（不是 127.0.0.1）
netstat -ano | grep LISTENING | grep ":8080 "
# 防火墙 profile
Get-NetFirewallProfile | Select-Object Name, Enabled
```

**关键**：手机里 `127.0.0.1` 是手机自己。所有指向电脑的地址必须写**局域网 IP**（如 192.0.2.78），且该 IP 可能变（路由器重连后），要能快速复核。

---

## 9. 排障决策树

```
插件没上线？
├─ Hub 上完全没有 agent
│   ├─ 容器→Hub TCP 不通 → 查 IP / 防火墙 / Hub 是否 0.0.0.0
│   ├─ 终端无响应 → 新建终端
│   └─ 日志有 failed to import → 是不是发了 TS 源码（坑 6）
├─ 日志里根本没有插件名
│   └─ 只点了「重启」→ 改「停止 → 启动」（§6.3）
└─ 上线了但连错地方
    └─ 配置 host 还是 127.0.0.1（坑 10）
```

```
命令敲了没反应？
├─ 先跑最简命令 'date>/sdcard/wb.txt' 验证通路
│   ├─ 不通 → 页面不在终端页 / 焦点不对 → 重进终端页 + 点 (600,1400)
│   ├─ 仍不通 → 旧会话失效 → 点 (1020,602) 新建
│   └─ 通了但我的命令不通 → 空格/重定向问题（坑 3、4）
```

---

## 10. 时间陷阱（最烧 token 的几处，直接跳过）

1. ❌ 不要尝试 `adb root` / `run-as` / 找 su / magisk —— 手机无 root，此路不通。
2. ❌ 不要在 3080/3081 token 认证上反复试参数名（token / access_token / bearer / x-token 全试过）—— 无效。
3. ❌ 不要用 HTTP 直链装插件 —— DSHA 只要 HTTPS。
4. ❌ 不要凭猜测点 UI 坐标 —— 必然落空且无报错。
5. ❌ 不要指望 uiautomator 能读到终端内容 —— 读不到，走文件回读。
6. ❌ 不要发布 TS 源码给 DSH 插件 —— node_modules 下无法 type-strip，必然 import 失败。
7. ❌ 不要用「重启 Web」刷新插件清单 —— 不生效，要「停止 → 启动」。

---

## 11. 通用模板：容器内诊断/验证脚本

```sh
#!/bin/sh
export DSH_HOME=/root/.dsh
OUT=/sdcard/diag.log
{
  echo "=== 身份 / 版本 ==="
  id -un
  dsh --version 2>&1 | head -2
  node -v

  echo "=== 插件是否在 bundles 中 ==="
  dsh --profile web --dump-config 2>&1 | grep -A6 "lan-hub"

  echo "=== 本次启动加载情况 ==="
  grep -iE "lan-hub|failed|did not activate" /root/dsh-web.log 2>/dev/null | tail -20

  echo "=== 容器 → Hub 连通性（用 node，别用 /dev/tcp）==="
  node -e "const n=require('net'),s=n.connect(8080,'192.0.2.78',()=>{console.log('CONNECT_OK');s.end()});s.on('error',e=>console.log('CONNECT_FAIL',e.code));s.setTimeout(3000,()=>{console.log('TIMEOUT');s.destroy()})"
} > $OUT 2>&1
```

调用：

```bash
"$ADB" -s "$T" push diag.sh /sdcard/diag.sh >/dev/null 2>&1
"$ADB" -s "$T" shell input tap 600 1400
"$ADB" -s "$T" shell "/system/bin/input text 'sh'"
"$ADB" -s "$T" shell input keyevent 62
"$ADB" -s "$T" shell "/system/bin/input text '/sdcard/diag.sh'"
"$ADB" -s "$T" shell input keyevent 66
sleep 8
"$ADB" -s "$T" shell "cat /sdcard/diag.log"
```

---

## 12. 现成脚本索引（`dsh/tools/`，直接用别重写）

全部脚本的输出都写回 `/sdcard/wb*.log`，调用方式统一见 §0/§11。

| 脚本 | 用途 |
|---|---|
| `wb_whoami.sh` | 容器内身份 / dsh / node 版本探测 |
| `wb_probe.sh` | 探查 `dsh plugin add` 用法与 profile 目录 |
| `wb_install.sh` | 从 tarball 安装插件 |
| `wb_reinstall.sh` | 重装（改用容器内部路径 `/root/local-plugins/`） |
| `wb_readcfg.sh` | 读回插件配置 + 测试手机→Hub 连通性 |
| `wb_config.sh` | 写入 id-targeted 配置（host 指向电脑 IP） |
| `wb_log.sh` | 读 `/root/dsh-web.log` 启动日志 |
| `wb_diag.sh` | 综合诊断（bundles / 加载情况 / 连通性） |
| `wb_import_err.sh` | 抓 `failed to import` 的详细错误 |
| `wb_ts_test.sh` | 验证 TS 源码在 node_modules 下是否可导入（本文结论：不可） |
| `wb_build.sh` | 容器内 `npm i esbuild` + 编译出 `dist/index.js` |
| `wb_extract.sh` | 把编译产物导出到 /sdcard 供 `adb pull` 拉回 |
| `wb_e2e.sh` | 端到端：加载插件、调用 8 个工具、与电脑端双向收发 |
| `wb_final.sh` | 最终状态核实 |
| `wb_deploy.sh` | **参数化**装指定版本插件：`sh wb_deploy.sh <版本> [profile]`。装前装后 dump profile 的 patch 全文 + 校验装进去的版本与关键能力特征 |
| `wb_cfg.sh` | **参数化**写配置：`sh wb_cfg.sh <版本> <agentId> <agentName>`。固化依赖源到 `/root/local-plugins/`、删掉旧 lan-hub 块并追加**字段写全**的配置块 |
| `wb_export_log.sh` | 把容器内 `dsh-web.log` 的关键段（版本/配置/加载行/lan-hub 行/错误）导出到 `/sdcard/lanhub.log`——**容器文件 adb 看不见，只能这么搬证据** |
| `wb_tools.sh` | 探查宿主已注册的工具清单（`dsh` CLI 没有 tools 子命令，这条路当时未走通，见 §13.4） |

> **多台手机的标准动作**（2026-10-24 实测顺序，脚本都已就位）：
> 1. `adb push` 三件套：`dsh-plugin-lan-hub-<版本>.tgz` + `wb_deploy.sh` + `wb_cfg.sh`
> 2. 终端注入 `sh /sdcard/wb_cfg.sh <版本> <agentId> <agentName>`
>    （这一步同时完成「装版本 + 固化依赖源 + 写配置」，不用再单独跑 deploy）
> 3. 启动页「停止 → 启动」（**必须**，见 §6.3）
> 4. `sh /sdcard/wb_export_log.sh` → `adb shell cat /sdcard/lanhub.log` 核对
> 5. PC 侧 `curl http://127.0.0.1:8081/api/agents` 确认该 agent 在线且 caps 全量

## 13. 本项目已验证的终态

```
容器内加载插件 → 8 个工具全部可用：
hub_broadcast, hub_create_room, hub_join_room, hub_leave_room,
hub_list_agents, hub_read_messages, hub_send, hub_send_room

手机 ↔ 电脑双向互通：
  电脑 → 手机：{"type":"chat","from":"pc","to":"wb-verify","content":{"text":"hello-from-pc-to-phone"},"seq":1}
  手机 → 电脑：{"type":"chat","from":"wb-verify","to":"pc-agent","content":{"text":"hello-from-phone"},"seq":2}
```

**实测：手机息屏后的自动重连行为**（无需人工干预）

```
[lan-hub] disconnected, reconnecting in 16000ms
[lan-hub] socket error: connect ETIMEDOUT 192.0.2.78:8080
[lan-hub] disconnected, reconnecting in 30000ms
[lan-hub] socket error: connect ETIMEDOUT 192.0.2.78:8080
[lan-hub] connected to 192.0.2.78:8080 as phone-agent   ← 自动恢复
```

> 插件日志前缀是 `[lan-hub]`。容器里 `grep '\[lan-hub\]' /root/dsh-web.log`
> 可完整看清连接生命周期（含退避间隔），是判断"断线还是没装好"的最快依据。

### 13.1 `host: auto` 局域网自动发现（插件 0.1.1 起，已真机验证；0.1.2 起带重试与诊断）

不用再填电脑 IP：把配置里的 `host` 设成 `auto`，插件会用 UDP 找 Hub。

配置文件 `/root/.dsh/profiles/web/cordis.patch.yml`（**整块替换，字段要写全**）：

```yaml
- id: lan-hub
  name: dsh-plugin-lan-hub
  config:
    host: 'auto'
    port: 8080
    agentId: 'phone-agent'
    agentName: 'PhoneAgent'
    heartbeatMs: 15000
    reconnect: true
    requestTimeoutMs: 3000
    inboxLimit: 200
    autoInject: false
    autoDiscover: true
    discoverPort: 8082
    fallbackHost: '192.0.2.78'
```

发现策略（三级兜底）：`255.255.255.255` 广播 → 各网卡 `<prefix>.255` 定向广播 → 本网段 `/24` 单播扫描。
部分 Wi-Fi 会过滤广播，靠第三级命中；`fallbackHost` 是最后保险。

成功的日志长这样（**关键就是第一行**）：

```
[lan-hub] discovered hub at 192.0.2.78:8080 (auto)
[lan-hub] connected to 192.0.2.78:8080 as phone-agent
[lan-hub] negotiated proto=3 heartbeat=15000ms features=[structuredAck,heartbeat,discovery,agentCaps,endpoints]
```

服务端交叉验证：`curl -s http://127.0.0.1:8081/api/agents` 应看到
`{"id":"phone-agent","proto":3,"caps":[5 项全量]}`。

> 若第一次尝试撞上瞬时未就绪，日志会多一行（**属正常自愈，不是故障**）：
> `[lan-hub] discovery attempt 1/2 failed: discovery timeout (ifaces=1, sent=254, recv=0)`
> 紧接着第二行就是 `discovered hub at ... (auto, attempt 2)`。

> **多台手机**：每台必须用**不同 `agentId`**（`phone-agent` / `huawei-agent` …），
> 否则在 Hub 上按 id 注册会互相顶掉，表现为「明明连上了却只剩一台在线」（坑 19）。

> **`dsh plugin add` 会触发插件热重载**：实测装完包 + 改完配置后，无需「停止→启动」，
> 日志就直接出现新的 `discovered hub at ... (auto)`。若改的是 bundle 层或不生效，再走 §6.3。

### 13.2 「插件激活失败但连接正常」——0.7.0 → 0.7.1 的真机取证与修复

**症状（最容易被判成「装好了」的一种故障）**：

```
[DSHA_STARTUP] 插件加载完成：dsh-plugin-lan-hub      ← 看着像成功
[lan-hub] connected to 192.0.2.78:8080 as phone1   ← 连接也成功
[lan-hub] negotiated proto=3 heartbeat=15000ms features=[...]
dsh: warning: 1 entry did not activate               ← 但插件其实没激活
lan-hub (dsh-plugin-lan-hub): JsonSchemaError: unsupported JSON schema:
    parameters.autoRespond.required must be true when present
```

Hub 在线表里 agent **照样在**（因为连接是在 `ctx.effect` 里先建立的），
但模型**一个工具都调不到**——10 个 `hub_*` 全部没注册。

**根因**：工具参数写成了 JSON Schema 的 `required: false`。
宿主 `@deepseek-ai/dsh-tools` 的 schema 编译器有硬规则：
**属性上只要带了 `required`，值就必须严格等于 `true`**。
可选字段的正确写法是**省略该键**，不是写 `false`。

**修法**：`dsh/src/index.ts` 里 `hub_config` 的 6 个可选参数删掉 `required: false`
（不能改成 `true`——那会让参数变必填，语义就变了）。

**回归测试**（`dsh/test/schema-check.ts`）：不自己写近似规则，
直接用宿主同款校验器 `@deepseek-ai/dsh-tools` 的 `assertSupportedJsonSchema`
去校验每个注册工具的 `parameters`，并断言 10 个工具齐全：

```bash
cd D:/AI-Agent/vibe/Agent-core-contect/dsh
node --experimental-strip-types test/schema-check.ts
# 反向验证：把 required:false 加回 autoRespond → 测试当场变红，
# 报错信息与真机日志逐字一致
```

**验收信号**（修复后在两台真机上都验过）：

```
[DSHA_STARTUP] 插件加载完成：dsh-plugin-lan-hub
[lan-hub] connected to 192.0.2.78:8080 as phoneN
[lan-hub] negotiated proto=3 heartbeat=15000ms features=[structuredAck,heartbeat,discovery,agentCaps,endpoints]
（没有 "1 entry did not activate"，也没有 JsonSchemaError）
```

> **通用教训**：DSHA 的启动日志只说「插件加载完成」，**真正的判据是
> "did not activate" 有没有出现**。装完插件一定要 grep 这一行，
> 光看「加载完成」会漏掉整类激活失败。

### 13.3 `host: auto` 在 proot 容器里可能发现不到（ifaces=0）

```
[lan-hub] discovery attempt 1/2 failed: discovery timeout (ifaces=0, sent=1, recv=0)
[lan-hub] discovery attempt 2/2 failed: discovery timeout (ifaces=0, sent=1, recv=0)
[lan-hub] discovery failed, fallback 192.0.2.78:8080
[lan-hub] connected to 192.0.2.78:8080 as phone2      ← 靠兜底连上
```

`ifaces=0` 表示插件在容器里枚举不到可用网卡（proot 环境的网卡枚举限制）。
此时 UDP 广播/单播全打空，**能连上完全是靠 `fallbackHost`**。

所以 `host: 'auto'` **必须同时配 `fallbackHost`**，否则在容器里等于完全连不上。
两台真机的表现也不一致：手机一 `ifaces=1` 能直接发现成功，手机二 `ifaces=0`
走兜底。**判定标准看日志里的 `discovered hub at` 还是 `fallback`**。

### 13.4 DSHA Web 端的 token（要查工具清单时才用得上）

Web 端（容器内 35039 / 3080）除 `/` 外全部要 token，且**DSHA 重启后 token 会变**：

```bash
# 1) 从启动页「启动日志」区拿当前 token（uiautomator dump 能读到）
"$ADB" -s "$T" shell "uiautomator dump /sdcard/ui.xml" >/dev/null 2>&1
"$ADB" -s "$T" shell "sed 's/></>\n</g' /sdcard/ui.xml" | grep -oE 'token=[-A-Za-z0-9_]+'

# 2) 转发到 PC 再访问（adb forward 会随会话丢失，必须同一条命令里做完）
"$ADB" -s "$T" forward tcp:13539 tcp:35039
curl -s "http://127.0.0.1:13539/api/tools?token=$TOK"
```

> 2026-10-24 实测：`?token=` / `Authorization: Bearer` / `X-Token` 三种传法
> 都返回 `unauthorized`，**工具清单这条路当时没走通**。判断插件是否激活，
> 用 §13.2 的日志信号更省事。

参考资料（PC 侧仓库，根 `D:/AI-Agent/vibe/Agent-core-contect`）：
- `dsh/README.md` —— DSHA 安装章节、配置写法
- `dsh/build.mjs` —— esbuild 编译（`external: @deepseek-ai/*`）
- `dsh/tools/wb_*.sh` —— 本文用到的全部容器脚本
