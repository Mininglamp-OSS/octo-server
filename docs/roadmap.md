# DMWork V1 Roadmap

## v1.1 — 安全修复 + 核心增强 ✅ 已完成
- [x] Bot token 吊销安全修复
- [x] WuKongIM managerToken 认证（5300 端口限制 127.0.0.1）
- [x] Android 文件安全修复 (#21 路径遍历, #22 大小/类型限制)
- [x] robotList API 权限升级 (#36 → #37)
- [x] Web emoji 显示修复 (#14 → dmwork-web#18)
- [x] 仓库迁移到 dmwork-org 组织
- [x] OpenClaw adapter 升级到 0.2.17

## v1.1.1 — Bug 修复 + 体验优化（2026-03-04）
- [x] Android 忘记密码验证码无效修复（code_type 参数缺失）
- [x] Android 文件消息"未知消息"修复（WKFileContent 注册）
- [x] Android 文本文件内置预览（yaml/json/md/代码文件等）
- [x] Android 未知格式文件保存到下载目录
- [x] Android Logo 更新
- [x] 搜索用户支持邮箱查找
- [x] APK 下载地址统一到主域名
- [x] 团队协作流程规范文档
- [x] 组织成员邀请（lml2468/研发、yeejiaa/产品）

## v1.2 — 功能完善（2026 年 Q2）
- [ ] adapter prompt 国际化 + timestamp 标准化 (dmwork-adapters#9)
- [ ] 待根据 Issue 需求排期

## 长期方向
- V1 保持稳定运行，服务现有用户
- 核心功能持续迭代
- V2 (DeepIM) 并行开发，逐步替代
