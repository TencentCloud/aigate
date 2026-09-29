# 我们为什么做 Tencent Cloud AIGate

[English](why-aigate.md) | [简体中文](why-aigate.zh-CN.md)

## 由 ClawPro 孵化

Tencent Cloud AIGate 是 **[ClawPro](https://cloud.tencent.com/document/product/213/129156)**
的模型接入层。ClawPro 是腾讯云开箱即用的一站式企业 AI 智能体管控平台：帮助企业
零代码、分钟级为全员规模化部署安全可控的专属数字助手——管理员集中管理成员与权限、
模型与知识库、主流 IM 通道（微信 / 企微 / QQ / 钉钉 / 飞书）与资源成本，并以
"能看见、可审计、强管控、全扫描"四层 AI 安全纵深防护覆盖智能体运行全链路。

> **了解更多 ClawPro** · [产品概述](https://cloud.tencent.com/document/product/213/129156)
> · [开通指南](https://cloud.tencent.com/document/product/213/129186)
> · [控制台](https://console.cloud.tencent.com/cvm/clawpro)

当 Agent 在企业规模上运行时，有一项要求不可妥协：**每一次模型调用都必须可治理**
——谁能调用哪个模型、用哪份上游凭据、受什么配额约束、留下什么审计记录；同时又
不能让团队额外维护一套代理集群、或把上游密钥散给使用者。这一层我们做在了
ClawPro 里；当它被验证具备普遍价值后，我们将其抽取为独立项目并开源：AIGate。

## 设计理念

AIGate 围绕一个问题而建：企业要真正运营好一个模型网关，究竟需要什么？答案
沉淀为它的设计理念：

- **治理内建，而非外挂。** 模型路由、四级 Token 配额、逐请求记账、密钥生命周期
  与 webhook 事件都是内核能力，而不是"企业版"才有的功能。
- **协议即契约。** 入站与上游都同时支持 OpenAI 与 Anthropic 协议：两侧一致时
  原生直通（`cache_control` 等 Anthropic 专有字段不丢失），不一致时完成请求、
  响应与流式的双向转换，不静默丢失语义。
- **像 CLI 一样运维，而不是像平台。** 单个静态二进制；笔记本用 SQLite、生产用
  MySQL；没有边车、没有代理集群。小团队能跑起来，企业也能标准化。
- **默认安全。** 所有出站请求经过 SSRF 加固（URL 校验、拨号时二次解析、重定向
  校验）；数据面密钥只存 SHA-256 哈希；配置主密钥后上游密钥加密落库。
- **可观测是内置属性。** 每一次请求都被记账；用量异步写入、永不阻塞转发；日志、
  日聚合、导出与签名 webhook 让消耗全程可审计。

## 生产验证

AIGate 不是拍脑袋的设计：它由经生产验证的 LLM proxy 抽取而来，其行为对应已经
跑过生产的请求模式与失败模式；开源前还针对真实上游服务完成了双协议族（流式与
非流式）的端到端验证。

## 延伸阅读

- [README](../README.zh-CN.md) —— 能力介绍与快速开始
- [API 参考](API.zh-CN.md) —— 数据面、管理面与用户面 API
- [部署](deploy.zh-CN.md) —— 配置、加密与 Docker
- [路线图](ROADMAP.zh-CN.md) —— 演进方向
