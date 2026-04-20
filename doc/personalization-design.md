# 个性化推送系统设计文档

> 状态：草稿，持续更新中

## 背景

用户目前没有明确的内容偏好，希望通过对文章主动打分/打标签来持续迭代个性化需求，最终实现更精准的推送。

---

## 目标

- 用户可以对任意文章打分（0-10）和打标签（正向/负向）
- 不强制要求每篇都打分，冷启动时系统正常工作
- 随着反馈积累，新文章的排序/过滤越来越符合用户口味
- 偏好模型可解释（用户能看到"为什么推这篇"）

---

## 用户交互设计（待补充）

- [ ] 打分方式：10分制
- [ ] 标签：正向标签（感兴趣）+ 负向标签（排斥）
- [ ] 标签本身也可以打权重（-1.0 ~ +1.0）
- [ ] 不是每篇都需要打分/打标，有反馈的才参与学习
- [ ] 前端入口：feed 卡片上的评分 UI（待确认具体 UI 形态）

---

## 数据流

```
[用户给文章打分/打标]
        ↓
[feedback 存储：feed_id → {score, pos_tags, neg_tags}]
        ↓
[偏好 Profile 计算（定期 or 有新反馈时触发）]
  ├── 高分文章(≥7) → embedding 平均向量 → "兴趣中心点"
  └── 标签权重表：{golang: +0.9, crypto: -0.8, ai: +0.6}
        ↓
[新文章入库时]
  ├── 计算与"兴趣中心点"的 cosine 相似度 → personal_score label
  └── 匹配标签权重 → 累加到 personal_score
        ↓
[推送时按 personal_score 排序 / 过滤低于阈值的内容]
```

---

## 架构设计

### 新增组件

| 组件 | 路径 | 说明 |
|------|------|------|
| Feedback API | `PATCH /api/feeds/{id}/feedback` | 接收用户评分和标签 |
| Feedback 存储 | KV namespace `user:feedback` | per-feed 持久化，不依赖 feed 本身生命周期 |
| Preference 计算器 | `pkg/preference/` | 定期计算偏好 profile（embedding 中心点 + 标签权重）|
| 偏好注入 rewrite | rewrite pipeline | 新文章写入时计算 `personal_score` label |
| 前端评分 UI | zenfeed-web，feed 卡片 | 10分 + tag 输入框 |

### API 设计（草稿）

```
PATCH /api/feeds/{id}/feedback
Body:
{
  "score": 8,           // 0-10，可选
  "tags": ["golang", "architecture"],     // 正向标签，可选
  "neg_tags": ["crypto", "marketing"]     // 负向标签，可选
}

GET /api/preference/profile
Response:
{
  "tag_weights": {"golang": 0.9, "crypto": -0.8},
  "feedback_count": 42,
  "last_updated": "2026-04-20T10:00:00Z"
}
```

### Preference Profile 数据结构

```json
{
  "liked_feed_ids": ["id1", "id2"],       // score >= 7 的文章
  "disliked_feed_ids": ["id3"],           // score <= 3 的文章
  "interest_vector": [0.1, -0.3, ...],   // 高分文章 embedding 均值
  "tag_weights": {
    "golang": 0.9,
    "ai": 0.6,
    "crypto": -0.8
  },
  "feedback_count": 42,
  "last_updated": "2026-04-20T10:00:00Z"
}
```

### personal_score 计算方式（草稿）

```
personal_score = 
  α * cosine_similarity(article_embedding, interest_vector)
  + β * sum(tag_weights[tag] for tag in article_tags)
  + (1 - α - β) * llm_score  // 原始 LLM 质量分
```

权重 α, β 可配置，冷启动（无反馈）时 α=β=0，只用 llm_score。

---

## 待讨论问题（用户还没说完）

- [ ] 前端 UI 具体形态（卡片内嵌？侧滑面板？）
- [ ] 负向反馈是否直接 drop_feed，还是只降权？
- [ ] 标签权重如何从历史打标中自动学习？
- [ ] 偏好 profile 多久更新一次？（每次有新反馈 / 每小时 / 每天）
- [ ] 是否需要"为什么推这篇"的可解释性展示？
- [ ] 用户是否希望手动调整标签权重，还是全部自动学习？
- [ ] 冷启动阶段（反馈 < N 条）的策略？

---

## 工程量估算

| 阶段 | 内容 | 估算 |
|------|------|------|
| Phase 1 | Feedback API + KV 存储 | ~200行 Go |
| Phase 2 | Preference 计算器 | ~200行 Go |
| Phase 3 | personal_score 注入 rewrite | ~50行 Go |
| Phase 4 | 前端评分 UI | ~150行 Svelte |
| Phase 5 | embedding 相似度排序 | ~100行 Go |

---

## 变更记录

| 时间 | 内容 |
|------|------|
| 2026-04-20 | 初稿，基于与用户的讨论 |
