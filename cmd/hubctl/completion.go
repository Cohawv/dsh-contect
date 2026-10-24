package main

import (
	"strings"
)

// completionBash 是 bash 补全脚本（静态，不依赖服务端）。
const completionBash = `# hubctl bash 补全 —— 由 "hubctl completion bash" 生成
# 用法：source <(hubctl completion bash)   # 或写入 ~/.bashrc
_hubctl() {
    local cur cmd i
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"
    local commands="status agents rooms room send say bcast plugins logs messages perf watch fmt completion help"
    local globals="--addr --fmt --color --help --version"

    cmd=""
    for ((i = 1; i < COMP_CWORD; i++)); do
        case "${COMP_WORDS[i]}" in
            -*) continue ;;
            *) cmd="${COMP_WORDS[i]}"; break ;;
        esac
    done

    if [[ -z "$cmd" ]]; then
        COMPREPLY=($(compgen -W "$commands $globals" -- "$cur"))
        return 0
    fi

    case "$cmd" in
        plugins)   COMPREPLY=($(compgen -W "start stop" -- "$cur")) ;;
        room)      COMPREPLY=($(compgen -W "create drop rename kick" -- "$cur")) ;;
        fmt)       COMPREPLY=($(compgen -W "json table" -- "$cur")) ;;
        completion) COMPREPLY=($(compgen -W "bash zsh powershell" -- "$cur")) ;;
        send)      COMPREPLY=($(compgen -W "--to --text --from" -- "$cur")) ;;
        say)       COMPREPLY=($(compgen -W "--room --text --from" -- "$cur")) ;;
        bcast)     COMPREPLY=($(compgen -W "--text --from" -- "$cur")) ;;
        agents)    COMPREPLY=($(compgen -W "-w --watch" -- "$cur")) ;;
        logs)      COMPREPLY=($(compgen -W "-f --follow -n --limit --level --since" -- "$cur")) ;;
        messages)  COMPREPLY=($(compgen -W "--since --limit --from --to --room" -- "$cur")) ;;
        perf)      COMPREPLY=($(compgen -W "-w --watch --fail-on-drop" -- "$cur")) ;;
        watch)     COMPREPLY=($(compgen -W "-level --no-agents" -- "$cur")) ;;
        status|rooms|help) COMPREPLY=() ;;
        *)         COMPREPLY=($(compgen -W "$commands" -- "$cur")) ;;
    esac
    return 0
}
complete -F _hubctl hubctl
`

// completionZsh 是 zsh 补全脚本（静态）。
const completionZsh = `#compdef hubctl
# hubctl zsh 补全 —— 由 "hubctl completion zsh" 生成
# 用法：hubctl completion zsh > "${fpath[1]}/_hubctl" && compinit
_hubctl() {
  local -a commands
  commands=(
    'status:服务状态总览（GET /api/status）'
    'agents:Agent 表格，-w 监听 SSE'
    'rooms:房间列表（GET /api/rooms）'
    'room:房间管理（create/drop/rename/kick）'
    'send:点对点发送（POST /api/send）'
    'say:房间发送（POST /api/room）'
    'bcast:广播（POST /api/broadcast）'
    'plugins:插件列表与启停（/api/plugins）'
    'logs:日志查询与跟随（/api/logs）'
    'messages:历史消息（GET /api/messages）'
    'perf:性能快照：吞吐/丢弃/内存/GC（GET /api/metrics/summary）'
    'watch:合并跟随事件流（agents -w + logs -f）'
    'fmt:查看或指定输出格式（json|table）'
    'completion:生成 shell 补全脚本'
    'help:显示帮助'
  )

  _arguments -C \
    '--addr[Hub REST 地址]:addr:' \
    '--fmt[输出格式]:fmt:(json table)' \
    '--color[彩色输出]:mode:(auto always never)' \
    '--version[显示版本]' \
    '(-h --help)'{-h,--help}'[显示帮助]' \
    '1: :->command' \
    '*:: :->args'

  case $state in
    command)
      _describe -t commands 'hubctl 子命令' commands
      ;;
    args)
      case $words[1] in
        plugins)   _values 'action' 'start' 'stop' ;;
        fmt)       _values 'format' 'json' 'table' ;;
        completion) _values 'shell' 'bash' 'zsh' 'powershell' ;;
        send)      _arguments '--to[目标 Agent]:to:' '--text[消息正文]:text:' '--from[发送方]:from:' ;;
        say)       _arguments '--room[目标房间]:room:' '--text[消息正文]:text:' '--from[发送方]:from:' ;;
        bcast)     _arguments '--text[消息正文]:text:' '--from[发送方]:from:' ;;
        agents)    _arguments '-w[监听模式]' '--watch[监听模式]' ;;
        logs)      _arguments '-f[跟随模式]' '--follow[跟随模式]' '-n[条数]:n:' '--level[级别]:level:(info warn error)' '--since[毫秒时间戳]:since:' ;;
        messages)  _arguments '--since[seq 游标]:since:' '--limit[条数]:limit:' '--from[发送方]:from:' '--to[接收方]:to:' '--room[房间]:room:' ;;
        perf)      _arguments '-w[监听模式]' '--watch[监听模式]' '--fail-on-drop[检测到丢弃时以退出码 3 结束]' ;;
        watch)     _arguments '--level[级别]:level:(info warn error)' '--no-agents[不打印 Agent 快照]' ;;
      esac
      ;;
  esac
}
compdef _hubctl hubctl
`

// completionPowerShell 是 PowerShell 补全脚本（静态）。
const completionPowerShell = `# hubctl PowerShell 补全 —— 由 "hubctl completion powershell" 生成
# 用法：hubctl completion powershell | Out-String | Invoke-Expression
Register-ArgumentCompleter -CommandName hubctl -Native -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)

    $commands = @('status', 'agents', 'rooms', 'room', 'send', 'say', 'bcast', 'plugins',
                  'logs', 'messages', 'perf', 'watch', 'fmt', 'completion', 'help')
    $globals = @('--addr', '--fmt', '--color', '--help', '--version')

    $tokens = $commandAst.CommandElements | ForEach-Object { $_.ToString() }
    $sub = $null
    foreach ($t in $tokens) {
        if ($t -eq 'hubctl') { continue }
        if ($t.StartsWith('-')) { continue }
        $sub = $t
        break
    }

    if (-not $sub) {
        ($commands + $globals) | Where-Object { $_ -like "$wordToComplete*" } | ForEach-Object {
            [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
        }
        return
    }

    $next = @()
    switch ($sub) {
        'plugins'    { $next = @('start', 'stop') }
        'room'       { $next = @('create', 'drop', 'rename', 'kick') }
        'fmt'        { $next = @('json', 'table') }
        'completion' { $next = @('bash', 'zsh', 'powershell') }
        'send'       { $next = @('--to', '--text', '--from') }
        'say'        { $next = @('--room', '--text', '--from') }
        'bcast'      { $next = @('--text', '--from') }
        'agents'     { $next = @('-w', '--watch') }
        'logs'       { $next = @('-f', '--follow', '-n', '--limit', '--level', '--since') }
        'messages'   { $next = @('--since', '--limit', '--from', '--to', '--room') }
        'perf'       { $next = @('-w', '--watch', '--fail-on-drop') }
        'watch'      { $next = @('--level', '--no-agents') }
        default      { $next = $commands }
    }

    $next | Where-Object { $_ -like "$wordToComplete*" } | ForEach-Object {
        [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
    }
}
`

// cmdCompletion 打印补全脚本。
//
// 补全脚本是"代码"而非"数据"，因此不受 -fmt 影响：无论当前格式是什么，
// 都原样输出脚本本身（用 -fmt json 拿到的仍是可 source 的脚本）。
func (a *app) cmdCompletion(args []string) error {
	if len(args) == 0 {
		return usageErr("hubctl completion bash|zsh|powershell（当前缺少 shell 名）")
	}
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "bash":
		return a.out.line(completionBash)
	case "zsh":
		return a.out.line(completionZsh)
	case "powershell", "pwsh":
		return a.out.line(completionPowerShell)
	default:
		return usageErr("不支持的 shell %q，可用：bash、zsh、powershell", args[0])
	}
}

// 下面这个小工具在测试里被调用：校验三个脚本都覆盖了全部子命令，
// 防止「补全里有、实际没有」或「新增子命令忘了补进补全」的漂移。
func missingFromScript(script string) []string {
	var missing []string
	for _, n := range commandNames() {
		if !strings.Contains(script, n) {
			missing = append(missing, n)
		}
	}
	return missing
}
