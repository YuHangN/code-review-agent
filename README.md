# Code Review Agent

一个本地运行的 Go CLI：输入 GitHub PR，生成带置信度、证据链和预算记录的中文 Markdown Code Review 报告。

## 核心能力

| 要求 | 实现 |
| --- | --- |
| 可恢复 | Run、Review Unit、Checker 和 Agent Step 写入 SQLite；中断后只继续未完成部分 |
| Token 预算 | 每次模型调用前预留、调用后结算；预算不足时按配置降级模型，最终跳过 Unit |
| 可观测 | 每条 Finding 关联 Trace，记录脱敏 Diff、Prompt、模型回复和工具结果 |
| 置信度 | `go vet/staticcheck` 命中新增行时为高置信度；纯 LLM 推理统一标为仅供参考 |
| 安全 | 固定 PR 的 base/head SHA；Secret Scanner 全链路脱敏；Checker 在受限 Docker 容器中运行 |
| 可扩展 | Tool 通过接口和 YAML 注册；SCM 通过 Adapter Registry 接入 |

## 快速运行

环境要求：Go 1.26、Docker，以及可读取目标仓库的 GitHub Token 和 OpenAI API Key。

```bash
git clone https://github.com/YuHangN/code-review-agent.git
cd code-review-agent

export GITHUB_TOKEN="..."
export OPENAI_API_KEY="..."

# 首次运行需要构建包含 go vet 和 staticcheck 的 Checker 镜像
make checker-image

mkdir -p bin
go build -o bin/review-agent ./cmd/review-agent

# 审查真实 PR，预算上限为 100 cents（$1）
bin/review-agent run --budget-cents 100 \
  https://github.com/YuHangN/code-review-agent/pull/2
```

成功后终端会输出：

```text
run_id=run-xxxxxxxxxxxxxxxx
status=reported
report_path=out/run-xxxxxxxxxxxxxxxx.md
```

SQLite 默认保存在 `review-agent.db`，报告默认保存在 `out/<run-id>.md`。首次构建或本地缓存被清理时需要联网；如需主动刷新基础镜像，执行 `make checker-image-refresh`。

## 审查链路

```text
GitHub PR
  → 固定 base/head SHA，获取并脱敏 Snapshot
  → Planner 将 Diff 切成可恢复的 Review Unit
  → go vet / staticcheck 扫描固定版本仓库
  → Reviewer Agent 读取 Unit 和已知 Checker 诊断，按需调用受限工具
  → Aggregator 汇总、分级和去重
  → SQLite 保存进度、预算与 Trace
  → 输出中文 Markdown 报告
```

Checker 先运行，Reviewer 会收到已确认诊断，因此不会重复报告同一问题。Reviewer 只能产出候选问题，不能自行把结果提升为高置信度。

## 常用命令

```bash
# 查看进度和预算
bin/review-agent status <run-id>

# 从 checkpoint 恢复
bin/review-agent resume <run-id>

# 查看报告中某条 Finding 的完整证据链
bin/review-agent trace <trace-id>

# 从已保存结果重新生成报告，不调用模型
bin/review-agent report <run-id>
```

常用覆盖参数必须放在 URL 或 ID 前：

```bash
bin/review-agent run \
  --db review-agent.db \
  --config config/runtime.yaml \
  --tools-config config/tools.yaml \
  --budget-cents 100 \
  --output out/report.md \
  <github-pr-url>
```

- `config/runtime.yaml`：租约、预算、模型降级顺序、价格和 Checker 资源限制。
- `config/tools.yaml`：Agent 最大轮次、工具声明、权限和结果大小限制。

## 安全边界

- GitHub 内容始终按首次记录的 commit SHA 读取，避免恢复时混入新提交。
- 敏感文件会被排除，疑似 Token、JWT、连接串和密钥赋值会在进入 LLM 与落盘前脱敏。
- 不执行目标仓库的测试、Makefile、`go generate` 或任意 Shell 命令。
- 依赖准备只允许访问配置的 Go Proxy；真正执行静态检查时无网络、源码只读、非 root，并限制 CPU、内存、PID 和执行时间。
- 默认只生成本地 Markdown，不修改目标仓库。

## 项目结构

```text
cmd/review-agent/       CLI 入口
internal/app/           用例编排与组件初始化
internal/workflow/      状态推进、恢复、Unit 调度和租约
internal/scm/           GitHub Adapter 与统一 SCM 接口
internal/checker/       Docker Checker、诊断解析和 checkpoint
internal/review/        Reviewer Agent Loop 与工具调用
internal/aggregation/   Finding 分级、去重和汇总
internal/store/sqlite/  状态、预算与 Trace 持久化
config/                 运行时和工具配置
prompts/                编译进二进制的版本化 Prompt
```

## 当前范围

- 已支持：GitHub PR、Go Checker、OpenAI、Markdown 报告。
- 暂未支持：GitLab MR、直接发布 PR 评论、多租户和后台 Worker。
