package gameserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"time"

	"rf4server/internal/store"
)

type catchReceipt struct {
	SkillGain                                        *float64       `json:",omitempty"`
	Bonuses                                          []catchXPBonus `json:",omitempty"`
	BaseXP, TotalXP, PremiumXP, Silver, PreviousBest int64
	Level, AccumulatedXP, LevelsGained               int64
	IsRecord                                         bool
	SkillID                                          int64
	CaughtAt                                         float64
	DroppedCardGUID                                  string                             `json:",omitempty"`
	Qualification                                    map[int64]store.QualificationState `json:",omitempty"`
}

func (r catchReceipt) result(replayed bool) *settleXpResult {
	gain := 0.2
	if r.SkillGain != nil {
		gain = *r.SkillGain
	}
	return &settleXpResult{baseXP: r.BaseXP, totalXP: r.TotalXP, premiumXP: r.PremiumXP, bonuses: r.Bonuses, skillGain: gain, silver: r.Silver,
		prevBest: r.PreviousBest, newLevel: r.Level, totalXp: r.AccumulatedXP,
		levelsGained: r.LevelsGained, isRecord: r.IsRecord, skillID: r.SkillID, replayed: replayed,
		droppedCardGUID: r.DroppedCardGUID, qualChanges: r.Qualification}
}

type catchWear struct {
	row        *store.Item
	fields     string
	durability float64
	log        store.ItemDamageLog
}

// prepareCatchWear 只准备字段快照；结算事务提交前不改物品或损伤历史。
func (s *Server) prepareCatchWear(ctx context.Context, uid int64, rigGUID string) ([]catchWear, error) {
	if rigGUID == "" {
		return nil, nil
	}
	var out []catchWear
	seen := map[string]bool{}
	add := func(guid, key string, slot, ordinal int64) error {
		if guid == "" || seen[guid] {
			return nil
		}
		seen[guid] = true
		row, err := s.DB.GetItemByGUID(ctx, guid)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if row != nil && row.UID != uid {
			return fmt.Errorf("钓组物品不属于当前玩家")
		}
		amount := wearPerCatch[key]
		if row == nil || amount <= 0 || row.Durability <= 0 {
			return nil
		}
		durability := math.Max(0, row.Durability-amount)
		fields, err := FieldsFromJSON(row.FieldsJSON)
		if err != nil {
			return err
		}
		idx := fieldIdxByCS(row.WireClass, csDurability)
		if idx >= 0 && !setWireFloat(fields, row.WireClass, idx, durability) {
			return fmt.Errorf("耐久字段写入失败: %s", row.WireClass)
		}
		bumpCreatedTicks(fields, row.WireClass)
		encoded, err := FieldsToJSON(fields)
		if err != nil {
			return err
		}
		before := row.Durability
		out = append(out, catchWear{row: row, fields: encoded, durability: durability,
			log: store.ItemDamageLog{UID: row.UID, ItemGUID: row.GUID, ItemID: row.ItemID,
				Event: damageEventWear, BeforeDurability: &before, AfterDurability: &durability,
				DetailJSON: damageDetailJSON(map[string]any{"slot": slot, "ordinal": ordinal, "category": key})}})
		return nil
	}
	components, err := s.DB.ListRigComponents(ctx, rigGUID)
	if err != nil {
		return nil, err
	}
	for _, component := range components {
		key, ok := persistedSlotWearKey[component.Slot]
		if !ok {
			key = "default"
		}
		if err := add(component.GUID, key, component.Slot, component.Ordinal); err != nil {
			return nil, err
		}
	}
	if err := add(rigGUID, "rod", gearCatRod, 0); err != nil {
		return nil, err
	}
	return out, nil
}

// settleCatchOnce 用鱼 GUID 记账，经验、熟练度、磨损和回执一起提交。
func (s *Server) settleCatchOnce(ctx context.Context, uid int64, f *RolledFish, rigType int64) (*settleXpResult, error) {
	return s.settleCatchOnceWithQualification(ctx, uid, f, rigType, nil)
}

// settleCatchOnceWithQualification 把资格判定与经验、磨损、回执放进同一事务：
// 资格写失败会回滚整笔结算，客户端重试时重新判定；回执同时保存本尾鱼的资格
// 变化，重放可补发丢失的增量响应，且按鱼 GUID 去重不会重复计数。
func (s *Server) settleCatchOnceWithQualification(ctx context.Context, uid int64, f *RolledFish, rigType int64, ev *qualificationEvent) (*settleXpResult, error) {
	if f == nil || f.FishGUID == "" {
		return nil, fmt.Errorf("缺少钓获鱼标识")
	}
	key := fmt.Sprintf("catch-settlement:%d:%s", uid, strings.ToLower(f.FishGUID))
	decodeReceipt := func(raw string) (*settleXpResult, error) {
		var saved catchReceipt
		if err := json.Unmarshal([]byte(raw), err != nil {
			return nil, err
		}
		return saved.result(true), nil
	}
	if raw, _, err := s.DB.KVGet(ctx, key); err == nil {
		return decodeReceipt(raw)
	} else if err != sql.ErrNoRows {
		return nil, err
	}
	if _, err := s.DB.EnsureProfile(ctx, uid); err != nil {
		return nil, err
	}
	xp, _ := catchValue(f.Species, f.Grade, f.WeightG)
	silver := ruCatchSilver(f.Species, f.Grade, f.WeightG, f.LocLevel, 0)
	xp = int64(math.Round(float64(xp) * s.tunableF("xp_gain", 1)))
	if xp < 1 {
		xp = 1
	}
	state := s.fishingStateForFish(uid, f)
	state.mu.Lock()
	gear := state.GearGuid
	if !f.HappyHourKnown {
		if sess := s.liveSessionByUID(uid); sess != nil {
			f.HappyHour = sess.currentHappyHour().Active
		}
		f.HappyHourKnown = true
	}
	happy := f.HappyHour
	bait := state.Bait
	state.mu.Unlock()
	bonuses, err := s.rodXPBonuses(ctx, uid, gear, xp)
	if err != nil {
		return nil, err
	}
	wear, err := s.prepareCatchWear(ctx, uid, gear)
	if err != nil {
		return nil, err
	}
	if sess := s.liveSessionByUID(uid); sess != nil {
		sess.updateHappyHour(time.Now(), false)
	}
	if happy {
		// 57 才是客户端结算面板的「欢乐时光」加成行；56 是另一类加成，客户端不会显示。
		bonuses = append(bonuses, catchXPBonus{Kind: 57, GUID: zeroGUID, Multiplier: .5, XP: int64(math.Round(float64(xp) * .5))})
	}
	card, err := s.prepareCatchCard(ctx, uid, f, bait)
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.SQL.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// 先取得写锁，同一尾鱼的并发请求只能有一次入账。
	if _, err := tx.ExecContext(ctx, `UPDATE kv SET v=v WHERE k=?`, key); err != nil {
		return nil, err
	}
	var existing string
	if err := tx.QueryRowContext(ctx, `SELECT v FROM kv WHERE k=?`, key).Scan(&existing); err == nil {
		return decodeReceipt(existing)
	} else if err != sql.ErrNoRows {
		return nil, err
	}
	var oldLevel, oldXP, membership, membershipExpiresAt int64
	if err := tx.QueryRowContext(ctx, `SELECT level,xp,membership,membership_exp FROM profile WHERE uid=?`, uid).
		Scan(&oldLevel, &oldXP, &membership, &membershipExpiresAt); err != nil {
		return nil, err
	}
	premiumXP := int64(0)
	if store.MembershipAt(membership, membershipExpiresAt, time.Now().Unix()).Active {
		premiumXP = xp
	}
	if xp > math.MaxInt64-premiumXP {
		return nil, fmt.Errorf("钓获经验超出范围")
	}
	totalXP := xp + premiumXP
	if bonus, err := s.drinkXPBonusTx(ctx, tx, uid, xp, time.Now()); err != nil {
		return nil, err
	} else if bonus != nil {
		bonuses = append(bonuses, *bonus)
	}
	for _, bonus := range bonuses {
		if bonus.XP > math.MaxInt64-totalXP {
			return fmt.Errorf("钓获经验超出范围")
		}
		totalXP += bonus.XP
	}
	if oldXP > math.MaxInt64-totalXP {
		return fmt.Errorf("累计经验超出范围")
	}
	newXP := oldXP + totalXP
	level := levelForXp(newXP)
	if level < oldLevel {
		level = oldLevel
	}
	if level > maxLevel {
		level = maxLevel
	}
	if base := baseXp(level); newXP < base {
		newXP = base
	}
	r := catchReceipt{Bonuses: bonuses, PremiumXP: premiumXP, BaseXP: xp, TotalXP: totalXP, Silver: silver, Level: level,
		AccumulatedXP: newXP, LevelsGained: max64(0, level-oldLevel), SkillID: 1}
	r.SkillID = fishingSkillCatForRig(rigType)
	var oldSkill float64
	if err := tx.QueryRowContext(ctx, `SELECT MIN(points,100.0) FROM skill WHERE uid=? AND skill_id=?`, uid, r.SkillID).Scan(&oldSkill); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	gain := math.Min(fishingProficiencyGainForRoll(oldSkill, happyHourProficiencyRoll(oldSkill, rand.Float64(), happy), rand.Float64()), math.Max(0, profCap-oldSkill))
	r.SkillGain = &gain
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(weight_g),0) FROM catch_log WHERE uid=? AND fish_key=?`, uid, f.Species).Scan(&r.PreviousBest); err != nil {
		return nil, err
	}
	r.IsRecord = r.PreviousBest <= 0 || f.WeightG > r.PreviousBest
	now := float64(time.Now().UnixNano()) / 1e9
	r.CaughtAt = now
	if _, err := tx.ExecContext(ctx, `UPDATE profile SET xp=?,level=?,updated_ts=? WHERE uid=?`, newXP, level, now, uid); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO txn(uid,ts,kind,silver_delta,gold_delta,note) VALUES(?,?,'xp',0,0,?)`,
		uid, now, fmt.Sprintf("xp+%d level %d->%d", totalXP, oldLevel, level)); err != nil {
		return nil, err
	}
	if gain > 1e-9 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO skill(uid,skill_id,points) VALUES(?,?,?)
			ON CONFLICT(uid,skill_id) DO UPDATE SET points=MIN(points+excluded.points,100.0)`, uid, r.SkillID, gain); err != nil {
			return nil, err
		}
	}
	for _, change := range wear {
		updated, err := tx.ExecContext(ctx, `UPDATE item SET fields_json=?,durability=?
			WHERE guid=? AND uid=? AND fields_json=? AND durability=?`, change.fields,
			change.durability, change.row.GUID, uid, change.row.FieldsJSON, change.row.Durability)
		if err != nil {
			return nil, err
		}
		if count, err := updated.RowsAffected(); err != nil || count != 1 {
			return nil, fmt.Errorf("钓组物品已变化，请重试结算")
		}
		if err := store.AddItemDamageLog(ctx, tx, change.log); err != nil {
			return nil, err
		}
	}
	if card != nil {
		if err := insertItemTx(ctx, tx, card); err != nil {
			return nil, err
		}
		r.DroppedCardGUID = card.GUID
	}
	if ev != nil {
		qev := *ev
		qev.XP = totalXP
		changes, err := evaluateQualificationsTx(ctx, tx, uid, qev, float64(time.Now().Unix()))
		if err != nil {
			return nil, err
		}
		r.Qualification = changes
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO kv(k,v) VALUES(?,?)`, key, string(raw)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// 官方升级礼物：真实升级（非重放）后逐级直发背包，内部按等级幂等；
	// 钓法大类取本尾鱼（11 级分套依据）。礼物行随结果返回，由 14/4 处理器
	// 挂结算奖励列表（801 f8）并在响应后推 4/22；聊天播报在发放函数内完成。
	res := r.result(false)
	if r.LevelsGained > 0 {
		for _, g := range s.grantLevelUpGifts(ctx, uid, oldLevel, level, r.SkillID) {
			res.giftItems = append(res.giftItems, g.Items...)
		}
	}
	// 稀有鱼 / 超级稀有鱼的鱼种数奖座**不在这里发**：图鉴口径是 catch_log，
	// 而 catch_log 要到 14/5 入护才写。结算阶段发会让「拿到奖座后放生」的玩家
	// 出现「背包有奖座、面板不计数」的不一致，改由 fishingKeep 入护后判定。
	return res, nil
}
