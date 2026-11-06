# dsh-sidecar

运行在 dsh 服务器上的独立中继。它让**远程浏览器**能正常使用 `dsh web`，并把 ReMe 状态页也带过去，
而 dsh 与 ReMe 仍然只监听 `127.0.0.1`。

## 它解决什么问题

dsh 的设计假设"浏览器就在本机"：

1. `/api` 与页面要求一个绑定 Host 的签名 Cookie（`dsh-auth-<hash>`），只有从 `?token=` 换取；
2. 客户端把"是否本机"写进页面逻辑，远程访问时部分功能会降级；
3. ReMe 状态页在浏览器里直接 fetch `http://127.0.0.1:2333`——远程浏览器访问的是自己的回环。

中继在服务端扮演"浏览器"，把这三件事补上，再把结果以普通 HTTP 暴露给 nginx。

```
浏览器 ──▶ nginx(TLS) ──▶ dsh-sidecar :8088 ──▶ dsh web  127.0.0.1:3080
                                          └──▶ ReMe    127.0.0.1:2333  （仅 /reme/*）
```

## 它做了什么

| 行为 | 说明 |
|---|---|
| 代签内核 Cookie | 读 `.credentials.yaml` 里的会话密钥，按内核格式签发 `dsh-auth-*`，代浏览器通过 `/api` 校验 |
| Host/Origin 归一化 | 转发前改写为上游 authority，满足内核的 loopback trust fence |
| 页面注入 | 在 `index.html` 的 `<head>` 后注入两段脚本：`ownsHost=true`（让远程页面按本机处理）与 ReMe fetch 改写（`127.0.0.1:2333` → 同源 `/reme/*`） |
| 透传 | HTTP、静态资源、HEAD、404、查询串、WebSocket 升级 |
| `/reme/*` 准入 | 校验浏览器带来的 `dsh-auth-*`，只放行状态页用到的作业 |

**它不做的事**：不提供 TLS，不提供登录。认证仍由 `@xgone/dsh-remote` 负责——中继只是把它的会话
原样转给 dsh，并借用它的登录结果保护 `/reme/*`。

## 快速开始

```sh
go run .                                   # 默认 127.0.0.1:8088 → dsh 127.0.0.1:3080
go run . -listen 0.0.0.0:8088 -v           # 局域网调试
```

依赖已 vendor，构建不需要联网：`go build -mod=vendor`。

## 配置

优先级：**命令行参数 > YAML 配置文件 > 内置默认值**。

### 命令行

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-listen` | `127.0.0.1:8088` | 监听地址 |
| `-upstream` | `http://127.0.0.1:3080` | dsh web 地址 |
| `-reme` | `http://127.0.0.1:2333` | ReMe 地址 |
| `-dsh-home` | `$DSH_HOME` 或 `~/.dsh` | 密钥目录 |
| `-credentials` | 空 | 直接指定 `.credentials.yaml`，优先于 `-dsh-home` |
| `-cookie-ttl` | `168h` | 代签 Cookie 有效期，内核上限 30 天 |
| `-inject` | `true` | 是否注入页面脚本 |
| `-reme-auth` | `true` | `/reme/*` 是否要求有效内核 Cookie |
| `-reme-readonly` | `false` | 禁止浏览器触发 `auto_dream`（立即整理） |
| `-v` | `false` | 打印每个请求 |
| `-config` | 空 | YAML 配置文件路径 |

### YAML

默认自动探测 `${XDG_CONFIG_HOME:-$HOME/.config}/dsh-sidecar/config.yaml`，存在即加载；
`-config` 指定后必须存在，否则启动报错。

```yaml
listen: 127.0.0.1:8088
upstream: http://127.0.0.1:3080
reme: http://127.0.0.1:2333
credentials: /home/alice/.dsh/.credentials.yaml   # 或 dsh-home
cookie-ttl: 168h
inject: true
reme-auth: true
reme-readonly: false
verbose: false
```

完整示例见 `deploy/config.example.yaml`（安装后位于 `<prefix>/config.example.yaml`）。

### 密钥位置解析顺序

1. `-credentials` / `credentials`
2. `-dsh-home` / `dsh-home`
3. `$DSH_HOME`
4. `~/.dsh`

启动日志会打印最终使用的路径（不打印密钥本身）。

## 部署

```sh
sudo deploy/install.sh -u alice        # 构建 + 安装 + 启用并启动
sudo deploy/install.sh -u alice --no-start
sudo deploy/uninstall.sh -u alice      # 停止、禁用、移除单元（保留二进制）
sudo deploy/uninstall.sh -u alice --purge   # 连同安装目录删除
```

安装结果：

- `<prefix>/dsh-sidecar`、`<prefix>/README.md`、`<prefix>/config.example.yaml`（前缀默认 `/opt/dsh-sidecar`）
- `/etc/systemd/system/dsh-sidecar@.service`

单元是模板，实例名即用户名：`dsh-sidecar@alice.service`。它与 dsh 的单元
`deepseek-harness-bwrap@%i.service` 构成**单向依赖**：

| 指令 | 效果 |
|---|---|
| `Wants=` | 启动中继时把同名 dsh 实例一起拉起 |
| `After=` | 保证 dsh 先启动；端口就绪由 `ExecStartPre` 轮询确认 |
| `PartOf=` | dsh 停止/重启时，中继跟着停/重启 |

停止或重启中继**不会**影响 dsh。多用户场景下每个实例需要不同的 `-listen` 端口，用 drop-in 覆盖：

```ini
# /etc/systemd/system/dsh-sidecar@alice.service.d/port.conf
[Service]
Environment=SIDECAR_LISTEN=127.0.0.1:8089
```

### Arch / pacman

```sh
cd dsh-sidecar
makepkg -f
sudo pacman -U dsh-sidecar-0.1.0-1-x86_64.pkg.tar.zst
```

安装后：`/usr/bin/dsh-sidecar`、`/usr/lib/systemd/system/dsh-sidecar@.service`、
文档与示例配置在 `/usr/share/doc/dsh-sidecar/`。启用实例：

```sh
sudo systemctl enable --now dsh-sidecar@alice
```

依赖已 vendor，构建不需要联网；源码直接取自 PKGBUILD 所在目录，因此不用准备 tarball。

## 已知限制

- **没有 TLS、没有登录**：必须放在 nginx 之类的前端后面，并且只监听回环。
- **`/reme/*` 的准入借用 dsh-remote 的登录结果**：内核 Cookie 由 dsh-remote 在认证通过后签发，
  因此移除该插件后，存量 Cookie 到期即失效。
- **纯中继语义**：不设超时、不改写请求上下文。浏览器刷新/关闭会取消上游请求；
  中继重启也会中断正在转发的长作业（例如 `auto_dream`）。
- 中继持有内核会话密钥，等价于持有 dsh 的会话能力，部署时应按敏感服务对待（只读密钥、最小权限、独立用户）。

## 许可证

MIT，见 [LICENSE](LICENSE)。
