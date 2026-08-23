---
type: Task
title: "Task: manager-console-email-mfa"
description: Add email OTP MFA to the system management console login flow.
tags: ["auth", "mfa", "manager-console", "smtp"]
timestamp: 2026-08-22T00:00:00Z
slug: manager-console-email-mfa
upstream: self
source: self
---

# Task: manager-console-email-mfa

## Goal

为系统管理控制台登录增加邮箱 OTP 二次验证。MFA 开启时，管理端登录必须经过“密码校验 → Challenge → 显式发送 OTP → 验证 OTP → 最终签发 token”，确保有效的管理端 MFA 流程才能完成登录。

## Background

本任务只保护管理控制台的管理员密码登录端点及其配套 Challenge 路由：

- `/v1/manager/login`
- `/v1/manager/login/send`
- `/v1/manager/login/resend`
- `/v1/manager/login/verify`

系统设置开关、SMTP 配置校验、管理员邮箱维护、Challenge 生命周期、OTP 原子消费、审计、错误码、Swagger 和回归测试，仅在服务于上述管理端 MFA 流程时纳入范围。

本任务不把 MFA 扩展为普通用户平台的认证策略，也不负责普通用户 token 与管理端 token 的隔离。普通用户登录、OAuth、扫码、找回密码等路径不进入管理端 Challenge/OTP 流程。为保护管理端 OTP 的专用 keyspace，公开 `/v1/user/email/sendcode` 只接受 `Register`、`EmailLogin` 和 `ForgetLoginPWD` 三类普通 CodeType，并拒绝 `CodeTypeManagerLogin`；这是一条防止公开接口干扰管理端 MFA 的边界校验，不是普通用户平台 MFA。

## Load-bearing list

- MFA 默认关闭；策略关闭时保持现有管理端登录行为。
- 成功加载的系统设置快照中未配置 MFA 开关时按“关闭”处理；快照未就绪或开关值非法时按“不可用”处理并 fail-closed，不能回落为关闭后直接签发 token。
- MFA 开启时，密码成功只能创建 Challenge，不能直接签发 token；Challenge 有 15 分钟绝对截止时间，同一 UID 同时只有一个活跃 Challenge。
- OTP 必须明确发送成功并提交为可验证状态后才能消费；Challenge 过期、重放、并发复用或账号快照变化均不得签发 token。重发开始即作废旧验证码，发送失败也不能恢复旧码。
- MFA 开启、MFA 开启期间修改 SMTP，以及启动时加载已开启 MFA 配置时，执行当前流程所需的 SMTP 格式、完整性和真实可用性校验；同批修改按合并后的最终配置校验。
- 其他实例通过自动 reload 观察到 MFA/SMTP 有效配置变化后，各自执行一次有界 SMTP 预检；预检结果按配置代际发布，旧配置的异步结果不得标记新配置 ready，普通 reload tick 不重复发信。
- 通过显式 `Reload()` 观察到 MFA/SMTP 有效配置变化的实例，同样执行一次有界 SMTP 预检；已由管理设置写入路径完成的同步预检不重复发送。
- 启动 SMTP 预检失败时不 panic、不自动关闭 MFA，输出运维告警，管理端登录保持 fail-closed。
- 新建管理员必须提供有效邮箱，与 MFA 当前开关状态无关；这是保证管理员账号在 MFA 开启后不会因无邮箱而无法登录的创建约束。
- 管理员创建和邮箱维护接口会在应用层查询邮箱是否已被其他用户占用，并拒绝已占用邮箱；本任务不建立数据库全局唯一索引，也不承诺并发写入下的绝对唯一性或邮箱归属模型语义。
- 普通 CodeType 仍可通过其既有普通用户业务流程使用；普通验证码不进入管理端 Challenge 流程。为隔离管理端 CodeType，验证码、发送限流、失败计数和锁定 key 按 CodeType 分区，因此本任务不承诺旧的跨 CodeType 共享额度或内部 Redis key 命名保持不变。

## Out of scope

- 系统配置可用性恢复探测、周期性 SMTP 健康检查和系统配置自愈。
- 无配置变化时对启动预检失败的自动恢复探测、后台重试或告警闭环。
- SMTP 投递队列、重试系统以及最终到达确认。
- 系统配置并发修改基础设施、全局锁和多实例一致性。
- 配置事务提交成功但本实例 `Reload()` 失败时的即时收敛、配置基础设施故障恢复和跨实例一致性不在本任务范围内；服务仅输出告警并保留最后一次成功加载的本地快照，后续由现有自动 reload 重新尝试。
- 普通用户登录端点、OAuth、扫码登录、找回密码强化及普通平台 MFA。
- 数据库级管理员邮箱全局唯一约束、邮箱归属及身份模型语义；邮箱维护接口仅提供兼容存量管理员邮箱缺失的修复入口，并执行应用层占用检查。
- group、space、market 等业务授权。
- 角色变更、角色缓存失效以及普通会话撤销；MFA 仅在 Challenge 消费时复核账号仍具管理端角色。
- `octo-lib` 的修改。

## Acceptance

- 以本实例最后一次成功加载的系统设置快照为准：MFA 关闭时管理端登录保持原有行为；MFA 开启时未经 OTP 验证不得返回管理端 token。
- 只有最新、明确发送成功且原子消费成功的 OTP 能完成管理端登录；验证码不能重放或并发复用。
- SMTP 配置不完整或启动预检失败时，管理端登录 fail-closed，不因预检失败直接签发 token。
- 配置提交成功但本实例 `Reload()` 失败时只输出 warning，保留旧快照和旧 readiness；在下一次自动 reload 成功前，不保证本实例立即收敛到数据库中的最新 MFA 策略。若旧快照仍为关闭状态，管理端会继续执行旧的关闭策略；该配置基础设施故障不作为本任务的 MFA 流程验收项。
- 启动预检失败只产生告警，不 panic；预检失败后的持续探测、自动恢复和 readiness 管理由系统配置/邮件基础设施任务负责。
- 通过 `/v1/manager/user/admin` 新建管理员缺少有效邮箱时创建失败；首次启动创建的系统超管是存量 bootstrap 例外，必须在开启 MFA 前通过邮箱维护接口补齐邮箱。
- 管理员邮箱创建和维护遇到已被其他用户占用的邮箱时失败；数据库唯一索引和并发写入下的绝对保证不作为本任务验收项。
- 公开用户发码接口拒绝 `CodeTypeManagerLogin`，普通 CodeType 仍走各自既有业务流程且不能进入管理端 Challenge 流程。
- 多实例系统设置一致性、无配置变化时的恢复探测和自愈不作为本任务验收项；本任务仅验收实例观察到 MFA/SMTP 配置变化后的单次预检，以及当前快照下的 MFA fail-closed 行为。
