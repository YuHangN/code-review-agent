# Code Review Agent

一个本地运行的 Go CLI：输入 GitHub PR，生成带证据和 Trace 的 Markdown Code Review 报告。

这个项目重点解决的不是“让模型看一遍 diff”，而是 AI Review 在实际使用中的几个工程问题：中断恢复、费用上限、结果可追踪、置信度判断和敏感信息保护。

## 计划用法

```bash
review-agent run https://github.com/acme/payments/pull/42 \
  --budget-cents 1000 \
  --output out/report.md

review-agent resume <run-id>
review-agent status <run-id>
review-agent trace <trace-id>
review-agent demo
```

## 设计重点

- **可恢复**：Run 和 Review Unit 保存到 SQLite，重启后复用已经完成的工具和模型结果。
- **预算限制**：模型调用前预留费用，调用后按实际 token 结算；预算不足时降级模型、缩小上下文或跳过低风险文件。
- **Finding Trace**：每条评论能关联脱敏 diff、Prompt 版本、模型响应、工具结果和费用记录。
- **证据分级**：只有规则、AST 或受信工具能够验证的问题才标记为高置信度；只有模型推理的问题标记为仅供参考。
- **安全边界**：Secret Scanner 在模型调用和内容落盘前运行；首版不执行用户仓库中的构建或测试脚本。
- **可扩展工具**：工具通过统一接口和 YAML 注册，新增工具不修改 Review Workflow。
- **平台适配**：主流程处理统一的变更快照，GitHub/GitLab 差异留在各自 Adapter 中；首版实现 GitHub。

## 项目结构

当前已搭好目录骨架，业务实现会按模块逐步补齐：

```text
cmd/review-agent/          CLI 入口
internal/
  domain/                  核心领域模型
  workflow/                Review 状态机与恢复编排
  scm/                     GitHub、GitLab 和 Fake Adapter
  snapshot/ security/      Snapshot 与脱敏边界
  policy/ tools/ model/    策略、工具与模型网关
  budget/ review/ trace/   预算、Finding 与可观测性
  report/ store/sqlite/    Markdown 输出与本地持久化
config/                    Policy 和规则配置
prompts/                   版本化 Review / Verify Prompt
fixtures/                  离线 Demo 和测试数据
migrations/ scripts/       数据库迁移与演示脚本
docker/ examples/          容器和示例产物
```
