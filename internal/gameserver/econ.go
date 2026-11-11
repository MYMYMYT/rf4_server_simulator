package gameserver

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"time"

	"rf4server/internal/proto"
	"rf4server/internal/store"
	"rf4server/internal/wiregen"
)

// 经济结算、渔获价值和商店逻辑。

// valueRates 按鱼种和档位保存经验与银币速率。
type vgKey struct {
	species string
	grade   int64
}

var valueRates = map[vgKey][2]float64{
	{"crucian", 1}: {0.1200, 0}, {"crucian", 2}: {0.1710, 0.0563},
	{"c.roach", 1}: {0.1856, 0}, {"c.roach", 2}: {0.2990, 0.0990}, {"c.roach", 3}: {1.4996, 0.7495},
	{"perch", 1}: {0.1570, 0}, {"perch", 2}: {0.3569, 0.1176},
	{"s.bream", 1}: {0.3556, 0}, {"s.bream", 2}: {0.6247, 0.2065},
	{"c.carp", 2}:    {0.1770, 0.0574},
	{"a.sleeper", 1}: {0.7780, 0}, {"a.sleeper", 2}: {1.8710, 0.6190},
}

// catchValue 计算渔获经验和展示用银价。
// 银价只用于结算界面和 catch_log，不直接增加余额；银币在鱼市 20/2 卖鱼时入账。
func catchValue(species string, grade, weightG int64) (xp, silver int64) {
	rate, ok := valueRates[vgKey{species, grade}]
	xpRate, silverRate := 0.4, 0.0
	if ok {
		xpRate, silverRate = rate[0], rate[1]
	} else if grade >= 2 {
		silverRate = 0.1
	}
	w := weightG
	if w < 1 {
		w = 1
	}
	xp = int64(math.Max(1, math.Round(xpRate*float64(w))))
	silver = int64(math.Max(0, math.Round(silverRate*float64(w)/silverRateSeedDivisor)))
	return
}

// grantXp 增加经验并重算等级，返回新等级、累计经验和提升级数。
func (s *Server) grantXp(ctx context.Context, uid, xp int64) (int64, int64, int64) {
	p, err := s.DB.EnsureProfile(ctx, uid)
	if err != nil {
		return 1, 0, 0
	}
	oldLevel := p.Level
	newXp := p.XP + xp
	newLevel := levelForXp(newXp)
	_, _ = s.DB.SQL.ExecContext(ctx, `UPDATE profile SET xp=?, level=?, updated_ts=? WHERE uid=?`,
		newXp, newLevel, float64(time.Now().Unix()), uid)
	_ = s.DB.AddTxn(ctx, uid, "xp", "", "", 0, 0, "fishing")
	return newLevel, newXp, newLevel - oldLevel
}

// fishingEnd 14/4 收竿结算。
func (s *Server) fishingEnd(ctx *Ctx) ([]byte, bool) {
	if !s.requireDB(ctx) {
		return nil, false
	}
	uid := ctx.Sess.Userid
	st := s.stateFor(uid)
	st.mu.Lock()
	if st.biteTimer != nil {
		st.biteTimer.Stop()
	}
	st.CastActive = false
	pending := st.Pending
	hooked := st.Hooked
	rigType := st.RigType
	gearGUID := st.GearGuid
	loc, spot := st.Loc, st.Spot
	st.mu.Unlock()

	cctx := ctx.Sess.pushCtx()
	var stats813 *proto.Value
	var profileTree *proto.Value
	var skill312 *proto.Value
	hasFish := pending != nil && hooked

	levelsGained := int64(0)
	prevLevel := int64(1)
	if hasFish {
		s.recordHuntCatch(cctx, uid, loc, spot, pending)
		// 14/4 只结算经验；银币由鱼市 20/2 卖鱼入账。
		xp, _ := catchValue(pending.Species, pending.Grade, pending.WeightG)
		silver := ruCatchSilver(pending.Species, pending.Grade, pending.WeightG, pending.LocLevel, 0)
		bonusXp := s.tunableF("xp_gain", 1.0)
		xp = int64(math.Round(float64(xp) * bonusXp))
		prevBest, _ := s.DB.CatchLogMaxWeight(cctx, uid, pending.Species)
		newLevel, _, gained := s.grantXp(cctx, uid, xp)
		levelsGained = gained
		prevLevel = max64(1, newLevel-1)
		// 按实际钓组大类增长；概率随当前熟练度反比下降，满熟练度不再增加。
		skillID, skillGain := s.awardFishingProficiency(cctx, uid, rigType)
		// catch_log 只在 14/5 入护时写入；14/4 放生的鱼不进入统计和纪录。
		pending.XP = xp
		pending.Silver = silver
		// 对本次钓组组件施加钓获磨损。
		s.wearRigForCatch(cctx, st.GearGuid)
		st.mu.Lock()
		st.LastSkill = skillID
		st.mu.Unlock()

		stats813 = s.build813(cctx, uid, pending, prevBest, skillGain, levelsGained)
		profileTree, _ = s.BuildProfile307W(cctx, uid, worldTotalGameMin(time.Now()))
		skill312 = s.buildSingleSkill312(cctx, uid, skillID)
	}

	// 801（空竿也回完整 801，f0/f6/f7 = null）
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
	obj801, err := wiregen.MakeObj("caagocpofin", map[int]*proto.Value{
		0:  f0,
		1:  proto.U32(s.castDistanceBitsForRig(cctx, uid, gearGUID, rigType)),
		2:  proto.U32(f32Const801F2),
		3:  s.build5003(cctx, uid),
		4:  proto.Null(),
		5:  proto.Null(),
		6:  f6,
		7:  f7,
		8:  proto.Null(),
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

	// 结算后主动推送：升级 → 2/6；然后 2/1
	if hasFish && levelsGained > 0 {
		ctx.Sess.Push2_6(prevLevel)
	}
	if hasFish {
		ctx.Sess.Push2_1()
	}
	return body, true
}

// build813 搏鱼统计块（813 内嵌 310 + 25）。
func (s *Server) build813(ctx context.Context, uid int64, f *RolledFish, prevBest int64, skillGain float64, levelsGained int64) *proto.Value {
	// SPECIES_FIGHT_STATICS 鱼种静态组（未知鱼种按 species_weight 推导）
	st := s.settleStatics(ctx, f.Species)
	f0, f1, f2, f3, f5 := float64(st[0]), float64(st[1]), float64(st[2]), float64(st[3]), float64(st[5])
	f4 := f3 * (0.9 + rand.Float64()*0.2)
	// 银价（分）按官服鱼市价格表计算（地图 × 鱼种 × 档位 × 重量）。
	silverCents := ruFishPriceCents(f.Species, f.Grade, f.WeightG, f.LocLevel, 0, float64(time.Now().Unix()))

	obj310 := wiregen.MustMakeObj("kegeppimjbm", map[int]*proto.Value{
		0: proto.U64(uint64(f.XP)), // 单鱼经验（不是累计）
		1: proto.U32(uint32(max64(0, levelsGained))),
		2: proto.U32(uint32(stLastSkill(s, uid))),
		3: proto.F32(float32(skillGain)),
		4: proto.Null(),
		5: proto.U32(0),
		6: proto.Args(proto.S("31")),
	})
	keep25 := s.buildPending25(f, s.stateFor(uid), s.currentLevel(uid))
	// ndiejnnioni.f0..f6 是客户端按 U32 克重读取的整数，不能写 proto.F32；
	// 2000.0f 的位型 1157234688 会在鱼卡尺显示成“1157235 公斤”。
	return wiregen.MustMakeObj("ndiejnnioni", map[int]*proto.Value{
		0: proto.U32(uint32(f0)),
		1: proto.U32(uint32(f1)),
		2: proto.U32(uint32(f2)),
		3: proto.U32(uint32(f3)),
		4: proto.U32(uint32(f4)),
		5: proto.U32(uint32(f5)),
		6: proto.U32(uint32(prevBest)),
		7: proto.U64(uint64(silverCents)),
		8: obj310,
		9: keep25,
	})
}

func stLastSkill(s *Server, uid int64) int64 {
	st := s.stateFor(uid)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.LastSkill == 0 {
		return 1
	}
	return st.LastSkill
}

// buildSingleSkill312 单大类 312（801.f6 用）。
func (s *Server) buildSingleSkill312(ctx context.Context, uid, catID int64) *proto.Value {
	effects, _ := s.effectiveFishingSkills(ctx, uid)
	return s.skillCategory312(catID, effects)
}

// buildPending25 由 RolledFish 构造 25 条目（14/5 回包与 813.f9 共用）。
func (s *Server) buildPending25(f *RolledFish, st *FishState, levelKey string) *proto.Value {
	if f.LocLevel != "" {
		levelKey = f.LocLevel
	}
	e := &store.KeepnetEntry{
		EchoGUID: f.FishGUID,
		Species:  f.Species,
		NameZh:   f.Name,
		WeightG:  f.WeightG,
		LengthM:  f.LengthM,
		Cat:      f.Grade,
		KeepBait: st.Bait,
		TS:       float64(time.Now().UnixNano()) / 1e9,
	}
	return buildKeepnet25(e, levelKey, s.rarityOf25(f.Species), s.fishModelVariantFor(f))
}

// fishingKeep 14/5 入护。
func (s *Server) fishingKeep(ctx *Ctx) ([]byte, bool) {
	if !s.requireDB(ctx) {
		return nil, false
	}
	uid := ctx.Sess.Userid
	guids := deepGUIDs(ctx.Tree)
	want := ""
	if len(guids) >= 2 {
		want = guids[1]
	} else if len(guids) == 1 {
		want = guids[0]
	}
	cctx := ctx.Sess.pushCtx()
	st := s.fishingStateForRequest(ctx)
	st.finishMu.Lock()
	defer st.finishMu.Unlock()

	st.mu.Lock()
	pending := st.Pending
	if pending != nil {
		if want != "" && !strings.EqualFold(want, pending.FishGUID) {
			st.mu.Unlock()
			ctx.ReplyFailure("System.ArgumentException", "入护的鱼与本次钓获不一致")
			return nil, true
		}
		echo := pending.FishGUID
		kept := *pending
		if kept.LocLevel == "" {
			kept.LocLevel = s.currentLevel(uid)
		}
		keepLevel := kept.LocLevel
		// 钓法快照取入护当刻该竿的钓组类型；与 14/4 结算的熟练度大类同源
		// （fishingSkillCatForRig），保证统计页与技能页对同一条鱼口径一致。
		tackleCat := tackleCatForRig(st.RigType)
		capacity, err := s.keepnetCapacity(cctx, uid)
		if err != nil {
			st.mu.Unlock()
			ctx.ReplyFailure("System.IO.IOException", "护网容量未能读取，请重试")
			return nil, true
		}
		// 落库
		keptInNet, err := s.DB.KeepFishingCatchWithCapacity(cctx, &store.KeepnetEntry{
			EchoGUID: echo, UID: uid, Species: kept.Species, NameZh: kept.Name,
			WeightG: kept.WeightG, LengthM: kept.LengthM, Cat: kept.Grade,
			KeepLoc: st.Loc, KeepSpot: int64(st.Spot), KeepBait: st.Bait,
			KeepLevel: keepLevel, TS: float64(time.Now().UnixNano()) / 1e9,
			TackleCat: tackleCat,
		}, kept.XP, kept.Silver, capacity)
		if err != nil {
			st.mu.Unlock()
			ctx.Log.Warn("入护保存失败", "err", err)
			if errors.Is(err, store.ErrKeepnetFull) {
				ctx.ReplyFailure("System.InvalidOperationException", "护网已满，请先腾出位置")
			} else {
				ctx.ReplyFailure("System.IO.IOException", "入护未能保存，请重试")
			}
			return nil, true
		}
		st.Pending = nil
		st.CastActive = false
		st.Hooked = false
		st.Resolved = true
		st.persist.recoverCompleted, st.persist.recoverPending = false, false
		st.cancelBiteTimerLocked()
		st.cancelPendingTimerLocked()
		if keptInNet {
			st.Keepnet = append(st.Keepnet, &kept)
		}
		st.mu.Unlock()
		s.notifyFishKept(ctx.Sess, &kept)
		// 稀有鱼 / 超级稀有鱼鱼种数奖座：入护才计入图鉴（catch_log 刚落库），
		// 放生不计——与 307.f16 星号、3/17 图鉴、307.16 星号同一口径。奖座推 4/22 并播报。
		if keptInNet {
			if gifts := s.rarityMilestoneAfterKeep(cctx, uid); len(gifts) > 0 {
				pushRarityStatuettesOnKeep(ctx.Sess, gifts)
			}
		}
		// 钓获礼包：入护成功后按概率当场开箱（异步，避免拖慢入护回包）。
		s.scheduleLootBox(ctx.Sess, &kept)
		// 本尾已落 catch_log；14/4 结算卡 f7 的档案快照早于落库、不含本尾，
		// 在此补推新档案，等级旁星号（307.f16）即时计入新奖杯鱼种。
		ctx.Sess.Push2_1()
		// 水域奖杯：这一尾可能刚好补上该水域面板的最后一种鱼。
		//
		// 只在 keptInNet（本次真的写进 keepnet_items + catch_log）时判定：
		// 图鉴、307.f16 星号、3/17 面板都只认 catch_log，14/6 放生不写，所以
		// 「入护即判定」才保证「面板填满」与「拿到奖杯」严格等价（review P2）。
		if keptInNet {
			if trophy, terr := s.grantWaterwayTrophy(cctx, uid, keepLevel); terr != nil {
				ctx.Log.Warn("水域奖杯发放失败", "water", keepLevel, "err", terr)
			} else if trophy != nil {
				// 结算界面（801）早于入护，已经发不出去了；改用背包增量 + 系统播报，
				// 与稀有鱼奖座、福袋开箱同一通道。
				ctx.Sess.pushWaterwayTrophies([]*store.Item{trophy})
				name := s.lootItemName(cctx, trophy.ItemID)
				if name == "" {
					name = trophy.ItemID
				}
				ctx.Sess.pushSystemChat(fmt.Sprintf("集齐该水域全部稀有鱼：%s", name))
			}
		}
		out := proto.Args(proto.S("61"))
		if keptInNet {
			out.Items = append(out.Items, s.buildPending25(&kept, st, keepLevel))
		}
		body, err2 := proto.EncodeValue(build800(out, proto.Null(), proto.Null()), ctx.Sess.Table())
		if err2 != nil {
			return nil, false
		}
		// 入护后 catch_log 才写入本次鱼获，诱饵广度类成就在这一刻判定。
		s.evaluateAchievements(ctx, achievementEvent{
			Kind: evFishKept, DistinctBaits: s.distinctCatchBaits(cctx, uid),
		})
		return body, true
	}
	st.mu.Unlock()

	// 无 Pending：查已入护（重复入护）→ 回该鱼快照；否则护网全量
	if want != "" {
		if e, err := s.DB.KeepnetGet(cctx, uid, want); err == nil && e != nil {
			level := e.KeepLevel
			if level == "" {
				level = s.currentLevel(uid)
			}
			f25 := s.keepnetItemTree(cctx, e, level)
			body, err2 := proto.EncodeValue(build800(proto.Args(proto.S("61"), f25), proto.Null(), proto.Null()), ctx.Sess.Table())
			if err2 != nil {
				return nil, false
			}
			return body, true
		}
	}
	// 护网全量快照
	out := proto.Args(proto.S("61"))
	if list, err := s.DB.KeepnetList(cctx, uid); err == nil {
		for _, e := range list {
			level := e.KeepLevel
			if level == "" {
				level = s.currentLevel(uid)
			}
			out.Items = append(out.Items, s.keepnetItemTree(cctx, e, level))
		}
	}
	body, err := proto.EncodeValue(build800(out, proto.Null(), proto.Null()), ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	return body, true
}

// fishingRelease 处理 14/6 放生，同时处理蓝冠鱼命名。
// 请求形状为 args=[guid, guid, str]，最后一个字符串是鱼名。
func (s *Server) fishingRelease(ctx *Ctx) ([]byte, bool) {
	if !s.requireDB(ctx) {
		return nil, false
	}
	uid := ctx.Sess.Userid
	guids := deepGUIDs(ctx.Tree)
	st := s.fishingStateForRequest(ctx)
	st.finishMu.Lock()
	defer st.finishMu.Unlock()
	st.mu.Lock()
	pending := st.Pending
	if pending != nil && len(guids) > 0 && !strings.EqualFold(guids[len(guids)-1], pending.FishGUID) {
		st.mu.Unlock()
		ctx.ReplyFailure("System.ArgumentException", "放生的鱼与本次钓获不一致")
		return nil, true
	}
	loc := st.Loc
	rigType := st.RigType
	st.mu.Unlock()

	// 807.f1 的 811 礼物行：仅在本次放生真实升级且发放了礼物时非 null。
	giftRows := proto.Null()
	if pending != nil {
		cctx := ctx.Sess.pushCtx()
		// 只有蓝冠鱼可以命名，已命名的鱼不能改名。
		// 命名条件不满足时按普通放生处理，并记录日志。
		if name := releaseFishName(ctx.Tree); name != "" {
			fid := pending.NamedID
			if fid == "" {
				fid = pending.FishGUID
			}
			if ok, why := canNameFish(cctx, s.DB, pending.Grade, fid); ok {
				if ok2, why2 := s.insertNamedFish(cctx, uid, pending, fid, name, loc); ok2 {
					st.mu.Lock()
					pending.Name = strings.TrimSpace(name)
					pending.NamedID = fid
					st.mu.Unlock()
					ctx.Log.Info("蓝冠命名成功", "鱼种", pending.Species, "重量", pending.WeightG, "名字", pending.Name)
				} else {
					ctx.Log.Warn("蓝冠命名被拒", "原因", why2, "鱼种", pending.Species, "名字", name)
				}
			} else {
				ctx.Log.Warn("蓝冠命名被拒", "原因", why, "鱼种", pending.Species, "名字", name)
			}
		}
		// 放生奖励：base × release_xp_ratio（默认 0.5）
		base := pending.XP
		if base == 0 {
			b, _ := catchValue(pending.Species, pending.Grade, pending.WeightG)
			base = b
		}
		ratio := s.tunableF("release_xp_ratio", 0.5)
		bonus := int64(math.Round(float64(base) * ratio))
		result, gifts, replayed, err := s.releaseCatchOnce(cctx, uid, pending.FishGUID, bonus, rigType)
		if err != nil {
			ctx.Log.Warn("放生保存失败", "err", err)
			ctx.ReplyFailure("System.IO.IOException", "放生未能保存，请重试")
			return nil, true
		}
		st.mu.Lock()
		st.Pending = nil
		st.CastActive = false
		st.Hooked = false
		st.Resolved = true
		st.persist.recoverCompleted, st.persist.recoverPending = false, false
		st.mu.Unlock()
		if !replayed && pending.NamedID != "" {
			s.DB.NamedFishNoteRecatch(cctx, pending.NamedID)
		}
		if !replayed {
			// 放生同样在结算卡之后落 catch_log，补推新档案刷新等级旁星号
			// （与 14/4 路径同一时机）；与放生经验是否为 0 无关。
			ctx.Sess.Push2_1()
		}
		if !replayed && result.Bonus > 0 {
			ctx.Sess.Push2_9(100, uint64(bonus))
			if result.LevelsGained > 0 {
				ctx.Sess.Push2_6(max64(1, result.Level-1))
			}
		}
		// 放生路径的升级礼物：礼物本体推 4/22 同步背包（与该路径既有
		// Push2_9/Push2_1 同一时机）；礼物行随 807.f1（811 数组）下发，
		// 客户端结算弹窗据此渲染"新级别礼物"卡片栈。
		for _, row := range gifts {
			if row != nil {
				s.pushItemUpdate(uid, row)
			}
		}
		if len(gifts) > 0 {
			if rows, rerr := s.giftRows811Arg(cctx, result.Level, gifts); rerr != nil {
				ctx.Log.Warn("升级礼物行构造失败（807.f1 置空）", "err", rerr)
			} else {
				giftRows = rows
			}
		}
		s.notifyFishReleased(ctx.Sess, pending)
		s.evaluateAchievements(ctx, achievementEvent{
			Kind: evFishReleased, Grade: pending.Grade,
		})
	}
	// 响应 807：{null, [811礼物行], 307, null, null, null}；未升级时 f1=null
	//（官服未升级放流样本形态，见 monitor docs/LEVEL_UP_GIFT_CARDS.md）。
	profileTree, err := s.BuildProfile307W(ctx.Sess.pushCtx(), uid, worldTotalGameMin(time.Now()))
	if err != nil {
		profileTree = proto.Null()
	}
	obj807, err := wiregen.MakeObj("gnilpafigmn", map[int]*proto.Value{
		0: proto.Null(),
		1: giftRows,
		2: profileTree,
		3: proto.Null(),
		4: proto.Null(),
		5: proto.Null(),
	})
	if err != nil {
		return nil, false
	}
	body, err := proto.EncodeValue(obj807, ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	return body, true
}
