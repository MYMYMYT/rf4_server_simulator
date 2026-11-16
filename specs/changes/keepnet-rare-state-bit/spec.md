# 鱼市稀有鱼卡补紫色稀有位（25.f24）

Status: done

## Problem Statement

在鱼市卖鱼列表里，稀有鱼种（如食用蟹 `ed_crab`，静态表 rarity=1）的卡片只显示达标绿旗，不显示紫色稀有标；玩家的稀有鱼看起来和普通鱼一样。结算卡与图鉴的紫/粉标正常，说明鱼种稀有度（25.f4）本身有值，缺的是鱼市/鱼护物品卡读取的另一路标志。

## Solution

鱼护条目在 25.f24 的物品状态位里带上「稀有」位：稀有与超稀有鱼种都置 `bapnijnooje` 的 bit0（1=复古/稀有），普通鱼种清除该位。鱼市卖鱼列表、鱼护格这类物品卡即可按状态位画出紫色稀有标，同时不改变结算卡/图鉴继续走 f4 的紫/粉分级。

## User Stories

1. 作为玩家，我希望在鱼市卖鱼列表和鱼护里一眼看出稀有鱼种，以便挑着卖、留标本。
2. 边界：普通鱼种的卡片不得出现稀有标；导入快照里的旧状态位不能把普通鱼标成稀有。
3. 边界：蓝冠未命名鱼的可命名位（1<<22）与稀有位同时存在时互不覆盖。
4. 边界：结算卡与图鉴的紫/粉分级不受影响（仍按 f4 0/1/2）。

## Implementation Decisions

- `internal/gameserver/keepnet_state.go`：新增 25.f24 状态位常量与 `keepnetStateBits`，稀有位与蓝冠可命名位合并输出。
- `internal/gameserver/handlers_game.go`：`buildKeepnet25` 改用 `keepnetStateBits`；所有走它的路径（结算、护网、快照、交易卡、标本板）自动带上稀有位。
- `internal/gameserver/native_inventory.go`：导入快照路径与 f4 同理，按静态表 `species_rarity` 覆盖 25.f24 的稀有位——稀有鱼补上、普通鱼清掉旧位。
- 数据与迁移：不改表结构、不加迁移；静态表 `species_rarity` 已有 252 条，沿用。
- 兼容：客户端基线 4.0.25034；旧档在下一次 4/1 / 4/5 / 4/22 重建时自然获得新位，无需人工修档。回滚只需还原两处状态位计算。

## Testing Decisions

- 单元（`internal/gameserver/keepnet_rarity_test.go`）：`buildKeepnet25` 稀有鱼 f24 bit0=1；普通鱼 bit0=0；蓝冠未命名同时有 bit22；f4 仍按 0/1/2 输出。
- 存储（真实 SQLite 快照路径）：导入快照的旧 f24 被静态表覆盖——稀有鱼补位、删除 `species_rarity` 行后普通鱼清位。
- 期望值来源：wiregen `ecknbkpeeji.f24` 字段注释（`bapnijnooje`：1=复古/稀有）与既有蓝冠可命名位 4194304，不是用同一段代码复算。
- 回滚后 `TestKeepnet25RarityField` / `TestKeepnetItemTreeRarityOverride` 新增断言变红。

## Out of Scope

- 不改 25.f4 的语义与紫/粉分级；不改结算卡、图鉴、标本板各自的 f4 渲染。
- 不改鱼市价格、询价 302 响应形状。
- 不改客户端资源或图标。

## Further Notes

- `[事实]` 静态库与玩家运行库 `species_rarity` 均含 `ed_crab=1`；`keepnetItemTree` / `buildKeepnet25` 已下发 f4=1，结算卡与图鉴按 f4 渲染。
- `[推断]` 鱼市卖鱼列表与鱼护格走物品状态位（25.f24 / `bapnijnooje`）绘制稀有标，此前该位恒 0（仅蓝冠可命名位），因此稀有鱼在鱼市卡片上没有紫标。
- `[未验证]` 需要一次实机确认：打上本改动后，鱼市卖鱼列表的稀有鱼卡片出现紫色稀有标；普通鱼与结算卡表现不变。
- 现象来源：玩家截图（食用蟹 376g / 35.11 银币，右上只有绿色达标旗）。
