# Code Review Agent

一个本地运行的 Go CLI：输入 GitHub PR，生成带证据和 Trace 的 Markdown Code Review 报告。当前只注册 GitHub Adapter；GitLab MR 作为后续 TODO。

这个项目重点解决的不是“让模型看一遍 diff”，而是 AI Review 在实际使用中的几个工程问题：中断恢复、费用上限、结果可追踪、置信度判断和敏感信息保护。

## 计划用法

```bash
review-agent run --budget-cents 1000 \
  --tools-config config/tools.yaml \
  --output out/report.md \
  https://github.com/acme/payments/pull/42

review-agent resume <run-id> --output out/report.md
review-agent status <run-id>
review-agent trace <trace-id>
```

- `run`：固定 PR Snapshot，执行 Review 并生成 Markdown 报告。
- `resume`：从 SQLite checkpoint 继续中断的 Run。
- `status`：查看当前进度、预算和失败原因。
- `trace`：查看某条 Finding 使用的 diff、工具和模型证据。

真实 `run/resume` 从 `GITHUB_TOKEN` 和 `OPENAI_API_KEY` 环境变量读取凭据；凭据不会写入配置、SQLite、Trace 或报告。模型、价格和请求超时位于 `config/runtime.yaml`，Agent 上限和工具声明位于 `config/tools.yaml`。

首次运行前构建固定的 Go Checker 镜像：

```bash
make checker-image
```

`go vet` 和 `staticcheck` 读取固定 `head_sha` 的完整仓库，但只有能够精确映射到 PR 新增行的诊断才会进入报告。

`llm.fallback_order` 定义每轮模型调用的预算降级顺序。只有当前 Tier 无法预留预算时才尝试下一级；默认从 `gpt-5.4` 的 `strong` Tier 降到 `gpt-5.4-mini` 的 `economy` Tier，全部无法预留时才将 Unit 标记为 `skipped_budget`。

## 整体链路框架

```text
用户执行 run
  → main 把命令参数交给 CLI
  → CLI 识别 run，并解析预算和输出路径
  → Application 通过 SCM Registry 解析 URL、选择 Adapter，并驱动完整用例
  → 打开 SQLite 并检查数据库版本
  → 写入 created Run，立即向用户输出 run_id
  → Run 进入 fetching，GitHub Adapter 获取 base/head SHA 和 diff
  → Security 排除敏感文件并脱敏 diff
  → 原子保存 Snapshot，Run 进入 fetched
  → Planner 按文件和 diff hunk 切分 Review Unit，并跳过低价值文件
  → UnitKey 标识稳定的代码位置，UnitID 绑定当前 Run
  → 根据鉴权、SQL、并发和配置等关键词标记风险
  → 原子保存全部 Review Unit，Run 进入 planned
```

如果 Fetch 中断，数据库会保留 `fetching` 状态，可使用已有的 `run_id` 继续；进入 `fetched` 后，本次 Review 始终使用已经保存的 Snapshot。规划中断时会从 Snapshot 重新生成相同的 Unit，进入 `planned` 后则直接复用数据库中的计划。

## 设计重点

- **可恢复**：Run 和 Review Unit 保存到 SQLite，重启后复用已经完成的工具和模型结果。
- **预算限制**：每轮模型调用前预留费用，调用后按实际 token 结算；剩余预算不足时不发起调用并标记对应 Unit。
- **Finding Trace**：每条评论能关联脱敏 diff、逐轮 Prompt、模型响应和工具结果。
- **证据分级**：只有 EvidenceRule 或可信 Checker 提供确定性证据时才标记为高置信度；纯模型推理标记为仅供参考。
- **双链路 Finding**：LLM Candidate 经 Verifier 分级；`go vet/staticcheck` 的新增行诊断直接成为高置信度 Finding，最终统一去重。
- **安全边界**：Secret Scanner 在模型调用和内容落盘前运行；不执行测试、`go generate`、Makefile 或仓库脚本，静态检查只在受限 Docker 容器运行。
- **可扩展工具**：工具通过统一接口和 YAML 注册，新增工具不修改 Review Workflow。
- **平台适配**：主流程只依赖统一的 `ChangeRef + Adapter`；首版注册 GitHub，后续可在 Registry 中增加 GitLab 实现。

## Reviewer Agent

Reviewer 使用有边界的结构化 Tool-Calling Loop，不解析自由文本 ReAct：

```text
模型返回 tool_calls
  → Registry 校验声明、权限和参数
  → 工具读取固定 Snapshot 并脱敏
  → Observation 写入 checkpoint 和 Trace
  → 下一轮模型输出 findings 或继续调用工具
```

- 每个 Unit 默认最多 4 轮模型调用、6 次工具调用。
- `read_file` 只读取首次固定的 `head_sha`，拒绝路径穿越和敏感文件。
- `search_symbol` 首版只搜索固定且已脱敏的 PR diff。
- 每轮模型调用独立记账；恢复时复用已经完成的 Agent Step。
- Verifier 不主动调用模型或工具，只复核 LLM Reviewer 的 Candidate，并读取已落盘的 diff 与工具证据做确定性分类。

## Review 工作流

```text
PR URL
  → 创建可恢复的 Run
  → 获取并固定 base/head SHA Snapshot
  → 脱敏、切分并按风险排序 Review Unit
  → Reviewer 按需调用受限工具并提出候选问题
  → Docker 沙箱对固定 head SHA 运行 go vet 和 staticcheck
  → 只保留命中 PR 新增行的 Checker Diagnostic
  → Verifier 关联 diff 与工具证据，分类为 confirmed / advisory
  → 保存 checkpoint 和预算账本
  → 生成 Markdown 报告
```

- **Workflow** 管理任务状态、恢复和步骤顺序。
- **Reviewer** 通过结构化 Agent Loop 补充上下文并发现候选问题。
- **Verifier** 只用确定性规则决定问题是否为高置信度。
- **Checker** 对整个固定仓库做类型与静态分析，结果不经过 LLM Verifier。
- 每个 Unit 和 Agent Step 独立持久化；中断后只继续未完成部分。

## 项目结构

当前核心链路对应的主要目录：

```text
cmd/review-agent/          CLI 入口
internal/
  app/                     run/resume 用例与组件组装
  domain/                  核心领域模型
  workflow/                planned 之后的状态推进、Unit 调度和 lease
  scm/                     GitHub Adapter 与统一 SCM 边界
  security/                Snapshot 与落盘内容的脱敏边界
  llm/                     预算保护后的模型 Provider
  budget/                  模型费用预留与结算
  review/                  Reviewer 与单个 Unit 的 checkpoint 处理
  checker/                 固定源码物化、Docker 沙箱、诊断解析与 checkpoint
  tools/                   声明式 Registry 与固定 Snapshot 工具
  verifier/                Finding 证据校验和置信度分级
  report/                  Markdown 报告生成
  store/sqlite/            SQLite checkpoint 与账本
config/                    运行时、模型和价格配置
prompts/                   编译进二进制的版本化 Review Prompt
migrations/                SQLite schema migration
```
