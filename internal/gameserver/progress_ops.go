package gameserver

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"

	"rf4server/internal/proto"
	"rf4server/internal/store"
	"rf4server/internal/wiregen"
)

// 进度与钓获结算：
//   - xp 使用累计值，等级由 levelForXp 重算；写点时保证等级不下降，
//     并把 xp 回填到当前等级门槛。
//   - 有效会员与竿体经验加成按基础经验独立计算，随结算回执保存。
//   - 鱼档位由结算尺的达标、奖杯和蓝冠三条重量线决定。

// settleXpResult 保存一次钓获结算的经验分解和结算状态。
type settleXpResult struct {
	bonuses         []catchXPBonus
	droppedCardGUID string
	premiumXP       int64
	replayed        bool
	baseXP          int64                              // 技能倍率后的裸经验（分解中的"基础经验"）
	totalXP         int64                              // 310.f0 = 本条鱼总经验
	silver          int64                              // 展示用银价（catch_silver_cents 口径，不入账）
	prevBest        int64                              // 该鱼种已有纪录重量（813.f6）
	newLevel        int64                              // grant 后等级
	totalXp         int64                              // grant 后累计经验
	levelsGained    int64                              // 本次升级数（310.f1）
	isRecord        bool                               // 是否破个人纪录
	skillID         int64                              // 310.f2 钓法大类（1 浮钓 / 2 纺车 / 3 底钓 / 11 海钓）
	skillGain       float64                            // 310.f3 本次实际熟练度增量
	giftItems       []*store.Item                      // 本次结算直发的升级礼物（仅非重放；挂 801 f8 奖励列表）
	qualChanges     map[int64]store.QualificationState // 本尾鱼带来的资格变化（重放时从回执还原）
}

// grantXpMonotonic 增加累计经验并重算等级，保证等级不下降。
// 返回新等级、累计经验和本次提升的等级数。
func (s *Server) grantXpMonotonic(ctx context.Context, uid, xpGain int64) (int64, int64, int64) {
	p, err := s.DB.EnsureProfile(ctx, uid)
	if err != nil {
		return 1, 0, 0
	}
	oldLevel := p.Level
	newXp := p.XP + xpGain
	newLevel := levelForXp(newXp)
	// 重算等级只升不降，避免档案中的 xp 与 level 不一致时降级。
	if newLevel < oldLevel {
		newLevel = oldLevel
	}
	if newLevel > maxLevel {
		newLevel = maxLevel
	}
	// xp 不得低于当前等级门槛，否则后续按 xp 重算会再次降级。
	if b := baseXp(newLevel); newXp < b {
		newXp = b
	}
	_, _ = s.DB.SQL.ExecContext(ctx, `UPDATE profile SET xp=?, level=?, updated_ts=? WHERE uid=?`,
		newXp, newLevel, float64(time.Now().Unix()), uid)
	_ = s.DB.AddTxn(ctx, uid, "xp", "", "", 0, 0,
		fmt.Sprintf("xp+%d level %d->%d", xpGain, oldLevel, newLevel))
	return newLevel, newXp, max64(0, newLevel-oldLevel)
}

// settleCatchXP 计算并持久化一条钓获的经验、熟练度、纪录和装备磨损。
// 经验依次经过技能倍率和单调入账；prevBest 在写入 catch_log 前读取。
func (s *Server) settleCatchXP(ctx context.Context, uid int64, f *RolledFish, rigType int64) *settleXpResult {
	r, _ := s.settleCatchXPWithQualifications(ctx, uid, f, rigType, nil)
	return r
}

// settleCatchXPWithQualifications 在成功结算时一并返回本尾鱼的资格变化。
// ev 为 nil 时跳过资格判定（普通结算调用方）。
func (s *Server) settleCatchXPWithQualifications(ctx context.Context, uid int64, f *RolledFish, rigType int64, ev *qualificationEvent) (*settleXpResult, map[int64]store.QualificationState) {
	r, err := s.settleCatchOnceWithQualification(ctx, uid, f, rigType, ev)
	if err != nil {
		s.log.Warn("钓获结算未保存", "err", err)
		return nil, nil
	}
	st := s.fishingStateForFish(uid, f)
	st.mu.Lock()
	f.XP, f.Silver = r.totalXP, r.silver
	st.mu.Unlock()
	return r, r.qualChanges
}

// 磨损槽位使用 item category，而不是客户端序号。鱼线与前导按承力线计算，
// 钩、拟饵和浮漂分别使用各自的磨损率。
var persistedSlotWearKey = map[int64]string{
	catLine:  "line",
	catLead:  "line",
	catHook:  "hook",
	catLure:  "lure",
	catBait:  "bait",
	catFloat: "float",
}

func (s *Server) wearRigForCatchPersisted(ctx context.Context, rigGUID string) {
	if rigGUID == "" || s.DB == nil {
		return
	}
	applyWear := func(row *store.Item, key string, amount float64, slot, ordinal int64) {
		if row == nil || amount <= 0 {
			return
		}
		before := row.Durability
		newD := math.Max(0, before-amount)
		if math.Abs(newD-before) < 1e-9 {
			return
		}
		if err := s.persistItemDurability(ctx, row, newD); err != nil {
			s.log.Warn("钓获磨损写库失败", "guid", shortGUID(row.GUID), "err", err)
			return
		}
		detail := damageDetailJSON(map[string]any{"slot": slot, "ordinal": ordinal, "category": key})
		if err := s.DB.AddItemDamageLog(ctx, store.ItemDamageLog{
			UID: row.UID, ItemGUID: row.GUID, ItemID: row.ItemID, Event: damageEventWear,
			BeforeDurability: ptrFloat64(before), AfterDurability: ptrFloat64(newD), DetailJSON: detail,
		}); err != nil {
			s.log.Warn("钓获磨损历史写库失败", "guid", shortGUID(row.GUID), "err", err)
		}
		s.log.Info("钓获磨损", "guid", shortGUID(row.GUID), "slot", slot,
			"ordinal", ordinal, "类别", key, "耐久", fmt.Sprintf("%.4f->%.4f", before, newD))
	}

	comps, err := s.DB.ListRigComponents(ctx, rigGUID)
	if err == nil {
		seen := map[string]bool{}
		for _, c := range comps {
			if c.GUID == "" || seen[c.GUID] {
				continue
			}
			seen[c.GUID] = true
			key, ok := persistedSlotWearKey[c.Slot]
			if !ok {
				key = "default"
			}
			row, rerr := s.DB.GetItemByGUID(ctx, c.GUID)
			if rerr != nil || row == nil {
				continue
			}
			applyWear(row, key, wearPerCatch[key], c.Slot, c.Ordinal)
		}
	}
	// 竿本体不在 rig_component 中，单独施加结构磨损。
	if rod, rerr := s.DB.GetItemByGUID(ctx, rigGUID); rerr == nil && rod != nil {
		applyWear(rod, "rod", wearPerCatch["rod"], gearCatRod, 0)
	}
}

// persistItemDurability 同步 item.durability、fields_json 与版本戳；
// 只更新列会在后续物品快照中被字段值覆盖。
func (s *Server) persistItemDurability(ctx context.Context, row *store.Item, durability float64) error {
	if durability < 0 {
		durability = 0
	}
	if durability > 1 {
		durability = 1
	}
	fields, err := FieldsFromJSON(row.FieldsJSON)
	if err != nil {
		return err
	}
	idx := fieldIdxByCS(row.WireClass, csDurability)
	if idx >= 0 {
		if !setWireFloat(fields, row.WireClass, idx, durability) {
			return fmt.Errorf("耐久字段写入失败: class=%s idx=%d", row.WireClass, idx)
		}
	}
	bumpCreatedTicks(fields, row.WireClass)
	fj, err := FieldsToJSON(fields)
	if err != nil {
		return err
	}
	if err := s.DB.UpdateItemFields(ctx, row.GUID, fj, row.Qty, durability, row.Workmanship, row.RigGUID); err != nil {
		return err
	}
	row.FieldsJSON = fj
	row.Durability = durability
	return nil
}

// stLocOf、stSpotOf、stGearOf 分别读取玩家位置、钓点和手持钓组。
func stLocOf(s *Server, uid int64) string {
	st := s.stateFor(uid)
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.Loc
}

func stSpotOf(s *Server, uid int64) int64 {
	st := s.stateFor(uid)
	st.mu.Lock()
	defer st.mu.Unlock()
	return int64(st.Spot)
}

func stGearOf(s *Server, uid int64) string {
	st := s.stateFor(uid)
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.GearGuid
}

// speciesFightStatics 保存 813 结算使用的鱼种搏斗静态值；
// 第五项单独作为随机缩放的基础值。
var speciesFightStatics = map[string][6]int64{
	"crucian": {300, 1900, 3200, 3998, 3996, 3954},
	"c.roach": {220, 1200, 2000, 2498, 2100, 2473},
	"perch":   {280, 1600, 2700, 3477, 3000, 3312},
	"s.bream": {140, 800, 1200, 1396, 1296, 1205},
}

var speciesFightStaticsDefault = [6]int64{280, 1600, 2700, 3477, 3000, 3312}

// settleStatics 返回 813 结算块使用的鱼种静态组。
//
// 已知鱼种沿用内置值；其余鱼种按 species_weight 的奖杯线/蓝冠线推导，
// 避免回落到默认组（鲈鱼）导致结算界面把别的鱼的重量标准显示出来。
func (s *Server) settleStatics(ctx context.Context, species string) [6]int64 {
	// 调试开关：settle_scale_debug>=1 时对所有鱼种发固定假值，
	// 用于判断客户端结算比例尺是否读取服务端 813 的 f0..f3。
	if s.tunableF("settle_scale_debug", 0) >= 1 {
		return [6]int64{111, 222, 333, 444, 555, 666}
	}
	if st, ok := speciesFightStatics[species]; ok {
		return st
	}
	f1, f2 := int64(1600), int64(2700)
	if s.DB != nil {
		_, tro, sup := s.DB.GradeLines(ctx, species)
		if tro != nil && *tro > 0 {
			f1 = *tro
		}
		if sup != nil && *sup > 0 {
			f2 = *sup
		}
	}
	if f2 <= f1 {
		f2 = f1 * 5 / 3
	}
	// 达标线优先用 Wiki 的 Record（绿旗）实测值；没有数据时回退到内置表，
	// 最后才按奖杯线比例估算（仅用于缺少公开数据的鱼种）。
	f0 := int64(0)
	if v := fishRecordWeight(species); v > 0 {
		f0 = v
	} else if st, ok := speciesFightStatics[species]; ok {
		f0 = st[0]
	} else {
		f0 = int64(math.Round(float64(f1) * 0.173))
	}
	f3 := int64(math.Round(float64(f2) * 1.25))
	f4 := int64(math.Round(float64(f2) * 1.1))
	f5 := int64(math.Round(float64(f2) * 1.2))
	return [6]int64{f0, f1, f2, f3, f4, f5}
}

// buildSettle813 构造 813 搏斗统计块：f8 是 310 经验块，f9 是 25 鱼条目。
// 310.f1 保存本次升级数，310.f6 保存加成分解，813.f4 由第五项静态值随机缩放。
func (s *Server) buildSettle813(ctx context.Context, uid int64, f *RolledFish, r *settleXpResult) *proto.Value {
	statics := s.settleStatics(ctx, f.Species)
	f4 := int64(float64(statics[4]) * (0.9 + rand.Float64()*0.2)) // 向下取整
	// 813.f7 是该鱼银价（分），按官服鱼市价格表计算。
	silverCents := ruFishPriceCents(f.Species, f.Grade, f.WeightG, f.LocLevel, 0, float64(time.Now().Unix()))

	// 310 经验块：f0=总经验，f1=升级数，f2=钓法大类，f3=熟练度增量，f6=加成分解。
	// 重放回执只还原事实，不能再次触发熟练度提示；满级后尤其会误弹旧的 +0.x = 100%。
	levelsGained := int64(0)
	skillGain := r.skillGain
	if r.replayed {
		skillGain = 0
	} else {
		levelsGained = max64(0, r.levelsGained)
	}
	bonuses := proto.Args(proto.S("31"))
	for _, bonus := range r.bonuses {
		bonuses.Items = append(bonuses.Items, wiregen.MustMakeObj("ffjmgigkjdg", map[int]*proto.Value{
			0: proto.F32(float32(bonus.Multiplier)),
			1: proto.Guid(bonus.GUID),
			2: proto.U64(uint64(bonus.XP)),
			3: proto.U32(9),
			4: proto.U32(bonus.Kind),
			5: proto.U32(1),
		}))
	}
	if r.premiumXP > 0 {
		bonuses.Items = append(bonuses.Items, wiregen.MustMakeObj("ffjmgigkjdg", map[int]*proto.Value{
			0: proto.F32(1),
			1: proto.Guid(zeroGUID),
			2: proto.U64(uint64(r.premiumXP)),
			3: proto.U32(9),
			4: proto.U32(4),
		}))
	}
	obj310 := wiregen.MustMakeObj("kegeppimjbm", map[int]*proto.Value{
		0: proto.U64(uint64(r.totalXP)),
		1: proto.U32(uint32(levelsGained)),
		2: proto.U32(uint32(r.skillID)),
		3: proto.F32(float32(skillGain)),
		4: proto.Null(),
		5: proto.U32(uint32(max64(0, r.levelsGained))),
		6: bonuses,
	})
	keep25 := s.buildPending25(f, s.fishingStateForFish(uid, f), s.currentLevel(uid))
	// ndiejnnioni.f0..f6 必须按 U32 克重编码；写成 F32 位型会导致客户端显示错误。
	return wiregen.MustMakeObj("ndiejnnioni", map[int]*proto.Value{
		0: proto.U32(uint32(statics[0])),
		1: proto.U32(uint32(statics[1])),
		2: proto.U32(uint32(statics[2])),
		3: proto.U32(uint32(statics[3])),
		4: proto.U32(uint32(f4)),
		5: proto.U32(uint32(statics[5])),
		6: proto.U32(uint32(r.prevBest)),
		7: proto.U64(uint64(silverCents)),
		8: obj310,
		9: keep25,
	})
}

// fishingEndSettle 处理 14/4 收竿结算；空竿或未刺中时返回空结算 801。
func (s *Server) fishingEndSettle(ctx *Ctx) ([]byte, bool) {
	if !s.requireDB(ctx) {
		return nil, false
	}
	uid := ctx.Sess.Userid
	st := s.fishingStateForRequest(ctx)
	st.finishMu.Lock()
	defer st.finishMu.Unlock()
	st.mu.Lock()
	if st.biteTimer != nil {
		st.biteTimer.Stop()
	}
	st.CastActive = false
	pending := st.Pending
	hooked := st.Hooked
	rigType := st.RigType
	gearGUID := st.GearGuid
	bait := st.Bait
	st.mu.Unlock()
	// 收竿即终局：先把这一竿累计的渔轮磨损落库。
	s.settleFightReelWearState(s.dbCtx(), st, uid, "14/4")

	cctx := ctx.Sess.pushCtx()
	castProf := s.castDistanceForRig(cctx, uid, gearGUID, rigType)
	var stats813 *proto.Value
	var profileTree *proto.Value
	var skill312 *proto.Value
	var qualChanges map[int64]store.QualificationState
	hasFish := pending != nil && hooked

	if pending != nil && !hooked {
		// 鱼已咬钩但玩家未刺中就收竿：鱼会凭空消失，必须有反馈。
		// 用跑鱼模板（kind3/语气词）播报"跑掉了"，不再静默。
		ctx.Log.Info("14/4 未刺中收竿：咬钩未上钩，补发跑鱼提示",
			"fish", shortGUID(pending.FishGUID), "species", pending.Species)
		s.notifyFishEscape(ctx.Sess, pending, "收竿跑鱼")
	}
	var res *settleXpResult
	if hasFish {
		// 资格事件在结算前用咬钩快照装配；判定与经验/回执在同一事务提交。
		// 达标当尾的结算响应即带完成（增量下发），重放从回执还原同一增量。
		ev := s.qualificationEventForCatch(cctx, pending, rigType)
		res, qualChanges = s.settleCatchXPWithQualifications(cctx, uid, pending, rigType, &ev)
		if res == nil {
			ctx.ReplyFailure("System.IO.IOException", "钓获结算未能保存，请重试")
			return nil, true
		}
		stats813 = s.buildSettle813(cctx, uid, pending, res)
		profileTree, _ = s.BuildProfile307W(cctx, uid, worldTotalGameMin(time.Now()))
		skill312 = s.buildSingleSkill312(cctx, uid, res.skillID)
		st.mu.Lock()
		st.LastSkill = res.skillID
		st.mu.Unlock()
		// 奖杯/蓝冠与破纪录经聊天频道播报；普通鱼由结算卡展示。
		if !res.replayed {
			s.notifyFishLanded(ctx.Sess, pending, res.isRecord)
		}
	}

	// 801 即使空竿也返回完整对象，f0/f6/f7 为 null。
	f0 := proto.Null()
	if hasFish {
		f0 = proto.Args(proto.S("174"), stats813)
	}
	f6 := proto.Null()
	if hasFish && skill312 != nil {
		f6 = proto.Args(proto.S("121"), skill312)
	}
	f7 := proto.Null()
	if hasFish && profileTree != nil {
		f7 = profileTree
	}
	// 801 继承的 f8 是本次获得的物品。仅发送 4/22 不会把物品加入钓获结果的
	// 奖励列表；重放已入册的卡片也不能把它移回散卡库存。
	// 升级礼物与钓获卡片同列：礼物行来自结算直发（非重放才有）。
	grants := proto.Args(proto.S("61"))
	if hasFish && res != nil {
		for _, row := range res.giftItems {
			if row == nil {
				continue
			}
			if tree, terr := s.ItemToTree(cctx, row); terr == nil {
				grants.Items = append(grants.Items, tree)
			}
		}
	}
	if hasFish && res.droppedCardGUID != "" {
		card, err := s.DB.GetItemByGUID(cctx, res.droppedCardGUID)
		if err != nil {
			ctx.ReplyFailure("System.IO.IOException", "钓获卡片未能读取，请重试")
			return nil, true
		}
		if card.UID == uid && card.Container == tradingCardContainer {
			tree, err := s.ItemToTree(cctx, card)
			if err != nil {
				ctx.ReplyFailure("System.IO.IOException", "钓获卡片未能显示，请重试")
				return nil, true
			}
			grants.Items = append(grants.Items, tree)
		}
	}
	// 礼物行：801.f4 = 811 数组。结算阶段只有升级礼物一种来源（f0=1）；同数组
	// 并存多来源的渲染能力保留给水域奖杯等后续玩法。无礼物保持 null（官服未升级
	// 样本形态）。
	giftRows := proto.Null()
	if hasFish && res != nil {
		lines := make([]gift811Line, 0, 2)
		if len(res.giftItems) > 0 {
			lines = append(lines, gift811Line{source: giftSourceNewLevel, param: res.newLevel, items: res.giftItems})
		}
		if len(lines) > 0 {
			if rows, rerr := s.giftRows811(cctx, lines); rerr != nil {
				ctx.Log.Warn("礼物行构造失败（801.f4 置空）", "err", rerr)
			} else {
				giftRows = rows
			}
		}
	}
	obj801, err := wiregen.MakeObj("caagocpofin", map[int]*proto.Value{
		0:  f0,
		1:  proto.U32(proto.F32Bits(float32(castProf.DistanceM))),
		2:  proto.U32(f32Const801F2),
		3:  s.build5003FromStates(qualChanges, s.buildSeason5012()),
		4:  giftRows,
		5:  proto.Null(),
		6:  f6,
		7:  f7,
		8:  grants,
		9:  proto.Args(proto.S("61")),
		10: proto.Null(),
	})
	if err != nil {
		return nil, false
	}
	body, err := proto.EncodeValue(obj801, ctx.Sess.Table())
	if err != nil {
		return nil, false
	}

	// 结算后推送升级和最新档案；多级连升只推送一次。
	if hasFish && res != nil && !res.replayed && res.levelsGained > 0 {
		ctx.Sess.Push2_6(max64(1, res.newLevel-1))
		// 官方聊天模板播报升级（k00；f2 填等级的 f32 位模式）。
		ctx.Sess.pushLevelUpChat(res.newLevel)
	}
	if hasFish && !res.replayed {
		ctx.Sess.Push2_1()
	}
	// 上鱼事件交给成就规则判断一次；2/8 弹窗排在 801 应答之后。
	if hasFish {
		s.evaluateAchievements(ctx, achievementEvent{
			Kind: evFishLanded, WeightG: pending.WeightG, RigType: rigType, Bait: bait,
		})
	}
	if hasFish && res.droppedCardGUID != "" {
		if card, err := s.DB.GetItemByGUID(cctx, res.droppedCardGUID); err == nil && card.UID == uid && card.Container == tradingCardContainer {
			// 单条 4/22 增量不足以刷新开着的卡牌列表（upsert 不替换本地已有
			// 物品、视图重建滞后一代，2026-11-08 实机）：改走收藏册全量先删
			// 后推两代重同步（见 trading_card_sync.go）。
			sess := ctx.Sess
			ctx.AfterResponse(func() { s.resyncTradingCardsTwice(sess, uid) })
		}
	}
	// 礼物 4/22 与卡片同步：801 响应发送后再更新背包缓存，避免与结算快照竞态。
	if hasFish && res != nil && len(res.giftItems) > 0 {
		gifts := res.giftItems
		ctx.AfterResponse(func() {
			for _, row := range gifts {
				if row != nil {
					s.pushItemUpdate(uid, row)
				}
			}
		})
	}
	// 收竿时补挂同种库存，供客户端校验下一次抛竿；搏鱼中不补挂。
	// 补挂后的钓组和背包数量在 801 响应发送后同步，避免结算覆盖数量更新。
	if gearGUID != "" {
		if change, rerr := s.reloadBaitForCast(cctx, uid, gearGUID); rerr != nil {
			ctx.Log.Warn("收竿后补饵失败", "rod", shortGUID(gearGUID), "err", rerr)
		} else if len(change.Updated) > 0 || len(change.Removed) > 0 {
			sess := ctx.Sess
			ctx.AfterResponse(func() { sess.pushBaitChange(change) })
		}
	}
	return body, true
}
