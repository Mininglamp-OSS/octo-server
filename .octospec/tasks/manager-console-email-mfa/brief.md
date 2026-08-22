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

本任务只保护管理控制台登录端点及其配套 Challenge 路由：

- `/v1/manager/login`
- `/v1/manager/login/send`
- `/v1/manager/login/resend`
- `/v1/manager/login/verify`

系统设置、SMTP 配置校验、管理员邮箱维护、Challenge 生命周期、OTP 原子消费、审计、错误码、Swagger 和回归测试，仅在服务于上述管理端 MFA 流程时纳入范围。

## Load-bearing list

- MFA 默认关闭；策略关闭时保持现有管理端登录行为。
- MFA 开启时，密码成功只能创建 Challenge，不能直接签发 token。
- OTP 必须明确发送成功并提交为可验证状态后才能消费；Challenge 过期、重放、并发复用或账号快照变化均不得签发 token。
- MFA 开启、MFA 开启期间修改 SMTP，以及启动时加载已开启 MFA 配置时，执行当前流程所需的 SMTP 配置和可用性校验。
- 启动 SMTP 预检失败时不 panic、不自动关闭 MFA，输出运维告警，管理端登录保持 fail-closed。
- 普通用户验证码的既有行为和公开用户登录路径不因本任务改变。

## Out of scope

- 系统配置可用性恢复探测、周期性 SMTP 健康检查和系统配置自愈。
- 启动 SMTP 预检失败后的自动恢复 readiness、后台重试或告警闭环。
- SMTP 投递队列、重试系统以及最终到达确认。
- 系统配置并发修改基础设施、全局锁和多实例一致性。
- 普通用户登录端点、OAuth、扫码登录及公开平台验证码流程。
- group、space、market 等业务授权。
- 角色变更、角色缓存失效以及普通会话撤销。
- `octo-lib` 的修改。

## Acceptance

- MFA 关闭时管理端登录保持原有行为；MFA 开启时未经 OTP 验证不得返回管理端 token。
- 只有最新、明确发送成功且原子消费成功的 OTP 能完成管理端登录；验证码不能重放或并发复用。
- SMTP 配置不完整或启动预检失败时，管理端登录 fail-closed，不因预检失败直接签发 token。
- 启动预检失败只产生告警，不 panic；预检失败后的持续探测、自动恢复和 readiness 管理由系统配置/邮件基础设施任务负责。
- 普通用户平台接口行为不因本任务改变。
