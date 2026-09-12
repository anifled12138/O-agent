# 07 · Skills

状态：Proposed

## 定义

Skill 是可惰性加载的任务方法和知识资源，不是可执行权限。Axiom 兼容以 `SKILL.md`
为入口的目录，同时用 Plugin Release 管理安装、版本和来源。

## 结构

```text
skill-id/
├─ SKILL.md                 metadata + full instructions
├─ references/              optional, lazy resources
├─ templates/               optional
└─ scripts/                 optional; scripts are artifacts, not implicit authority
```

索引只保存 ID、名称、摘要、标签、适用条件、版本、digest 和来源。正文不进入默认系统
提示；Agent 通过 `skill.search` 和 `skill.load` 获取。

## 加载

1. Resolver 在当前用户、workspace、Agent preset 和 Grant 范围内搜索；
2. 返回有限数量 Skill Card；
3. load 固定 exact release 并读取完整 `SKILL.md`；
4. references 根据 Skill 路由说明按需读取；
5. loaded event 记录 release/digest/source；
6. Turn 结束后默认释放，Generation policy 可明确保持。

## 安全

- Skill 文本是非可信指令，不能覆盖 Kernel Policy；
- `scripts/` 不因被引用就自动执行，执行必须走 Process Tool 和 Grant；
- 路径限制在 Release root，拒绝 symlink/path traversal 逃逸；
- 大资源返回 Artifact handle；
- 网络引用安装时固定或显式声明，不在模型调用中静默拉取；
- Agent 生成 Skill 也走 PluginSpec、Git、测试、Release 和安装流程。

## Context 行为

Skill body 作为有来源的 context segment，拥有 token cost、priority、expiry 和 digest。
压缩时保留“使用了哪个 Skill release”的 provenance；需要精确内容时可重新展开。

## 测试

- metadata 解析和未知字段；
- summary/body 生命周期分离；
- reference lazy load；
- 路径逃逸与恶意指令；
- release 更新期间旧 Turn 固定；
- token budget 和大文件 handle。

