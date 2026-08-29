-- composite 统一账号池已取消「模型 → 目标平台」路由机制（见
-- .claude/artifacts/designs/composite-unified-pool.md）：网关链路不再读取
-- composite_model_routes，管理端接口与前端路由编辑器一并下线。
-- 表成为无人读取的死配置，按确认口径彻底移除（fallback_targets 列随表一起消失）。
DROP TABLE IF EXISTS composite_model_routes CASCADE;
