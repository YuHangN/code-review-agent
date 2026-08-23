# Code Review Agent

一个本地运行的 Go CLI：输入 GitHub PR，生成带证据和 Trace 的 Markdown Code Review 报告。

这个项目重点解决的不是“让模型看一遍 diff”，而是 AI Review 在实际使用中的几个工程问题：中断恢复、费用上限、结果可追踪、置信度判断和敏感信息保护。

## 计划用法

```bash
review-agent run https://github.com/acme/payments/pull/42 \
  --budget-cents 1000 \
  --output out/report.md

review-agent resume <run-id> --output out/report.md
review-agent status <run-id>
review-agent trace <trace-id>
```

- `run`：固定 PR Snapshot，执行 Review 并生成 Markdown 报告。
- `resume`：从 SQLite checkpoint 继续中断的 Run。
- `status`：查看当前进度、预算和失败原因。
- `trace`：查看某条 Finding 使用的 diff、工具和模型证据。

真实 `run/resume` 从 `GITHUB_TOKEN` 和 `OPENAI_API_KEY` 环境变量读取凭据；凭据不会写入配置、SQLite、Trace 或报告。模型、价格和请求超时配置位于 `config/runtime.yaml`。

## 设计重点

- **可恢复**：Run 和 Review Unit 保存到 SQLite，重启后复用已经完成的工具和模型结果。
- **预算限制**：模型调用前预留费用，调用后按实际 token 结算；预算不足时降级模型、缩小上下文或跳过低风险文件。
- **Finding Trace**：每条评论能关联脱敏 diff、Prompt 版本、模型响应、工具结果和费用记录。
- **证据分级**：只有规则、AST 或受信工具能够验证的问题才标记为高置信度；只有模型推理的问题标记为仅供参考。
- **安全边界**：Secret Scanner 在模型调用和内容落盘前运行；首版不执行用户仓库中的构建或测试脚本。
- **可扩展工具**：工具通过统一接口和 YAML 注册，新增工具不修改 Review Workflow。
- **平台适配**：主流程处理统一的变更快照，GitHub/GitLab 差异留在各自 Adapter 中；首版实现 GitHub。

## Review 工作流

```text
PR URL
  → 创建可恢复的 Run
  → 获取并固定 base/head SHA Snapshot
  → 脱敏、切分并按风险排序 Review Unit
  → 模型提出候选问题并写入脱敏 Trace
  → Verifier 使用确定性规则分类为 confirmed / advisory
  → 保存 checkpoint 和预算账本
  → 生成 Markdown；显式 publish 后才回评 PR
```

- **Workflow** 管理任务状态、恢复和步骤顺序。
- **Reviewer** 负责发现候选问题，不直接发布评论。
- **Verifier** 用规则和工具证据决定问题是否为高置信度。
- 每个 Unit 独立持久化；中断后只继续未完成部分。

## 项目结构

当前核心链路对应的主要目录：

```text
cmd/review-agent/          CLI 入口
internal/
  domain/                  核心领域模型
  workflow/                Review 状态机与恢复编排
  scm/                     GitHub、GitLab 和 Fake Adapter
  security/                Snapshot 与落盘内容的脱敏边界
  llm/                     预算保护后的模型 Provider
  budget/                  模型费用预留与结算
  review/                  Prompt 执行与候选 Finding
  verifier/                Finding 证据校验和置信度分级
  report/                  Markdown 报告生成
  store/sqlite/            SQLite checkpoint 与账本
config/                    运行时、模型和价格配置
prompts/                   编译进二进制的版本化 Review Prompt
migrations/                SQLite schema migration
```
