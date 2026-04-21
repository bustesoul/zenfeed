# RSS 抓取配置 UI 化方案

> 状态：草稿
> 范围：仅设计，不涉及本次代码改动

## Background

当前项目已经在配置模型中支持以下抓取配置：

- 全局默认抓取频率：`scrape.interval`
- 全局回溯窗口：`scrape.past`
- RSSHub 地址：`scrape.rsshub_endpoint`
- 单个源覆盖抓取频率：`scrape.sources[].interval`

并且配置保存后，后端已经支持热重载并立即生效：

- 前端通过 `/apply_config` 提交完整配置
- `ConfigManager.SaveAppConfig()` 写入配置文件后触发 reload
- `ScrapeManager.Reload()` 会根据新配置重建或重启对应 scraper
- source 未设置 `interval` 时，继承全局 `scrape.interval`
- 全局和单源都未设置时，默认抓取频率为 `1h`
- 抓取频率最小值被限制为 `10m`

因此，这个需求的本质不是新增后端能力，而是把已有配置能力做成更友好的 UI。

## Problem

当前 Sources 页面只支持：

- 添加 URL 源
- 添加 RSSHub Path 源
- 删除源
- OPML 导入

当前页面不支持：

- 查看全局默认抓取频率
- 修改全局默认抓取频率
- 为单个源设置抓取频率覆盖值
- 查看某个源当前“实际生效”的抓取频率
- 区分“继承全局”和“自定义覆盖”

这导致用户只能通过 Advanced Config 直接改 YAML，门槛较高，也不容易理解最终生效值。

## Goals

本方案目标：

- 将 RSS 抓取相关的常用配置从 YAML 暴露到 Sources UI
- 支持“全局默认值 + 单源覆盖值”的配置方式
- 在 UI 中明确显示每个源的“实际生效值”
- 保存后立即生效
- 不新增后端接口，继续复用现有 `query_config` / `apply_config`

## Non-Goals

本方案暂不包含：

- 输入即自动保存
- 为每个源配置独立 `past`
- 抓取失败监控、抓取状态历史、最后成功抓取时间
- RSSHub 分类、站点、路由发现能力改造
- 更换配置存储模型

## Design Principles

### 1. 配置分层明确

抓取配置采用两层结构：

- 全局默认值：适用于大多数源
- 单源覆盖值：只在特殊源上单独设置

### 2. UI 必须显示“最终生效值”

不能只显示原始配置值，必须显示：

- 当前源是否继承全局
- 当前源最终实际生效的频率

### 3. 保存即生效，而不是输入即生效

原因：

- 当前后端保存配置会触发 reload
- scraper 可能被重建
- 高频自动保存会造成不必要的 reload

因此采用：

- 本地编辑
- 点击保存
- 保存成功后立即生效并刷新显示

### 4. 尽量不引入新概念

第一版只暴露最关键的字段：

- `scrape.interval`
- `scrape.past`
- `scrape.rsshub_endpoint`
- `scrape.sources[].interval`

## UX Proposal

## 1. Sources 页面增加“全局抓取设置”区域

放在源列表上方，作为独立卡片。

建议字段：

- 默认抓取频率 `scrape.interval`
- 默认回溯窗口 `scrape.past`
- RSSHub Endpoint `scrape.rsshub_endpoint`

建议展示形式：

- 字段名
- 当前值
- 简短说明
- 保存按钮

示例：

- 默认抓取频率：`1h`
- 默认回溯窗口：`24h`
- RSSHub Endpoint：`http://rsshub:1200`

## 2. Source 列表显示“实际抓取频率”

在每个 source 行增加一列：

- 抓取频率：显示最终有效值
- 来源：显示 `继承全局` 或 `自定义`

示例：

- `1h · 继承全局`
- `15m · 自定义`

这样用户不进入编辑页也能知道当前行为。

## 3. 新增 / 编辑 Source 时支持频率覆盖

在 source 表单中增加：

- 开关：`跟随全局默认`
- 输入框：`抓取频率`

交互规则：

- 默认开启“跟随全局默认”
- 开启时，不写 `scrape.sources[].interval`
- 关闭时，允许输入如 `15m`、`30m`、`2h`
- 表单中实时显示：
  - 当前全局默认值
  - 当前源保存后预计生效值

## 4. 明确提示系统最小值限制

在频率输入框旁边提示：

- 最小抓取频率为 `10m`
- 小于 `10m` 时，系统会自动提升为 `10m`

避免用户误以为自己成功设置了 `1m`

## Data Semantics

## 1. 全局配置

```yaml
scrape:
  interval: 1h
  past: 24h
  rsshub_endpoint: http://rsshub:1200
```

## 2. 单源继承全局

```yaml
scrape:
  sources:
    - name: github-trending
      rss:
        rsshub_route_path: github/trending/daily
```

解释：

- 未设置 `interval`
- 实际抓取频率继承全局 `1h`

## 3. 单源覆盖全局

```yaml
scrape:
  sources:
    - name: github-trending
      interval: 15m
      rss:
        rsshub_route_path: github/trending/daily
```

解释：

- 当前源实际抓取频率为 `15m`
- 不受全局 `scrape.interval` 影响

## 4. 实际生效值计算规则

前端展示时建议使用如下逻辑：

```text
effective_interval =
  source.interval
  || scrape.interval
  || 1h
```

但最终以后端 reload 后的实际结果为准。

注意：

- 后端还会应用最小值限制 `10m`
- 因此前端本地预览值与后端最终值可能存在修正差异
- 保存成功后应重新拉取配置并刷新显示

## Save Flow

继续沿用当前配置保存机制：

1. 页面初始化调用 `query_config`
2. 前端在内存中修改完整 `AppConfig`
3. 点击保存时调用 `apply_config`
4. 后端写入配置并触发 reload
5. 前端保存成功后重新调用 `query_config`
6. 用后端返回值刷新 UI

这样可以保证：

- 展示的是后端真正接受后的配置
- 继承关系和最小值限制都能反映到 UI

## Validation Strategy

前端只做轻量校验：

- duration 字符串不能为空
- 不允许非法格式
- 对 `rsshub_route_path` 保持现有空格校验
- 如果输入 `< 10m`，给出提示但仍可提交，最终以后端修正为准

后端继续作为最终校验方。

## Suggested Scope

## Phase 1

只做以下内容：

- Sources 页面增加“全局抓取设置”
- Source 列表显示“实际抓取频率”
- 新增 source 时支持可选 `interval override`
- 编辑 source 时支持切换“继承全局 / 自定义覆盖”
- 保存后重新拉取配置并刷新显示

## Phase 2

后续可考虑：

- 显示最后一次抓取时间
- 显示最近一次抓取结果
- 支持手动触发单源立即抓取
- 支持 source 级别的更多参数
- 支持 RSSHub 路由浏览与自动选择

## Open Questions

- 是否需要在 source 列表中直接行内编辑频率，还是统一进入编辑表单？
- `scrape.past` 是否应该在第一版就暴露给普通用户？
- `rsshub_endpoint` 是否放在 Sources 页面，还是保留在更高级的设置区域？
- 新增 source 表单是否需要升级为“新增 / 编辑”共用弹层，而不是当前单向新增表单？

## Recommended Decision

建议采用以下第一版方案：

- 在 Sources 页面增加一个“全局抓取设置”卡片
- source 列表增加“生效频率”展示
- source 表单增加“跟随全局默认”开关和覆盖频率输入
- 保持保存即生效，不做自动保存
- 不新增接口，只复用 `query_config` / `apply_config`

这是当前架构下实现成本最低、收益最高、认知负担最小的方案。

## Implementation Plan

- 先补“全局抓取设置”可视化与保存能力
- 再补单源 `interval` 覆盖编辑
- 再补“生效频率”展示与保存后回显
- 最后再评估是否继续做抓取状态可视化

## Task List

- 为 Sources 页面补充 `scrape.interval` / `scrape.past` / `scrape.rsshub_endpoint` 编辑 UI
- 为 source 数据模型补充 `interval` 表单读写
- 在 source 列表中计算并展示最终生效频率
- 在表单中增加“继承全局 / 自定义覆盖”切换
- 保存后重新读取配置并回填 UI
- 增加基础校验与文案提示
- 补充 i18n 文案
- 补充最小值限制说明

## Thought in Chinese

这个需求本质上不是“给 RSSHub 加功能”，而是把已有配置语义做成更适合普通用户理解和操作的界面。

当前后端已经支持：

- 配置热重载
- 全局默认值
- 单源覆盖值
- 保存后立即生效

因此最合理的做法不是增加新的协议和状态，而是把“默认值、覆盖值、生效值”三件事在 UI 上讲清楚。

第一版一定要克制，不要把所有抓取相关配置一次性铺满。只做最关键的频率配置，就能显著提升可用性，同时不会把 Sources 页面变成一个小型 Advanced Config。
