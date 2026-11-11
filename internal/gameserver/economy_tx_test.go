package gameserver

import (
	"context"
	"encoding/json"
	"testing"

	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/proto"
	"rf4server/internal/store"
)

func newEconomyTxTestServer(t *testing.T) (*Server, *store.DB) {
	t.Helper()
	db := openItemTestDB(t)
	srv := NewServer(config.Game{Tunnel: config.Tunnel{PSK: "econ"}}, logging.Discard(), nil)
	srv.SetDB(db)
	return srv, db
}

func seedEconomyItemDef(t *testing.T, db *store.DB, numericID int64, itemID string, category int64) {
	t.Helper()
	if _, err := db.SQL.Exec(`INSERT OR REPLACE INTO item_def(numeric_id,item_id,category,kind,brand,attrs_json)
		VALUES(?,?,?,'','','{}')`, numericID, itemID, category); err != nil {
		t.Fatal(err)
	}
}

func economyTestItem(t *testing.T, srv *Server, uid int64, itemID, container string, qty int64) *store.Item {
	t.Helper()
	it, err := srv.prepareNewItem(context.Background(), uid, itemID, container, qty, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.DB.InsertItem(context.Background(), it); err != nil {
		t.Fatal(err)
	}
	return it
}

func economyTestCtx(srv *Server, uid int64, tree *proto.Value) *Ctx {
	sess := &Session{ID: 1, Userid: uid, Srv: srv, log: logging.Discard()}
	sess.closed.Store(true)
	return &Ctx{Sess: sess, CallID: 1, Tree: tree, DecodeOK: true, Log: logging.Discard()}
}

func economyCraftTree(recipe string, materials []craftSlot, qty uint64) *proto.Value {
	arg := proto.Args(proto.S("11"))
	for _, sl := range materials {
		arg.Items = append(arg.Items, proto.Obj("bhfikaplhmh", 9,
			proto.FStr("STR", proto.S(sl.ItemID)),
			proto.FGuid(sl.GUID),
			proto.FStr("STR", proto.S("")),
		))
	}
	return proto.Args524(proto.Obj("dhkphmjinmj", 8,
		proto.FStr("STR", proto.S(recipe)),
		proto.FVal("VAL", proto.Args(proto.S("11"))),
		proto.FVal("VAL", arg),
		proto.FOpt(0),
		proto.FStr("U16STR", nil),
		proto.FNum("U32", qty),
	))
}

func TestMarketSellAtomicAndDedup(t *testing.T) {
	srv, db := newEconomyTxTestServer(t)
	ctx0 := context.Background()
	uid := int64(2001)
	if _, err := db.EnsureProfile(ctx0, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`UPDATE profile SET silver=100 WHERE uid=?`, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO keepnet_items(echo_guid,uid,species,name_zh,weight_g,length_m,cat,keep_loc,keep_spot,keep_bait,keep_level,ts)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, "aa000000000000000000000000000001", uid, "ruffe", "梅花鲈", 300, 0.2, 2, "loc1_02", 1, "worm", "level_001_lake", 1.0); err != nil {
		t.Fatal(err)
	}
	ctx := economyTestCtx(srv, uid, proto.Args524(
		proto.Guid("aa000000000000000000000000000001"),
		proto.Guid("aa000000000000000000000000000001"),
	))
	body, ok := hMarketSell(ctx)
	if !ok || len(body) == 0 || body[0] == 0x0F {
		t.Fatalf("20/2 应成功: ok=%v body=%x", ok, body)
	}
	var silver int64
	if err := db.SQL.QueryRow(`SELECT silver FROM profile WHERE uid=?`, uid).Scan(&silver); err != nil {
		t.Fatal(err)
	}
	if silver <= 100 {
		t.Fatalf("卖鱼应入账且只入账一次，silver=%d", silver)
	}
	var txnCount, keepCount int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM txn WHERE uid=? AND kind='sell_fish'`, uid).Scan(&txnCount); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM keepnet_items WHERE uid=?`, uid).Scan(&keepCount); err != nil {
		t.Fatal(err)
	}
	if txnCount != 1 || keepCount != 0 {
		t.Fatalf("重复 GUID 不应重复结算: txn=%d keep=%d", txnCount, keepCount)
	}
}

func TestStoreCurrencyAndWalletAtomic(t *testing.T) {
	srv, db := newEconomyTxTestServer(t)
	uid := int64(2002)
	seedEconomyItemDef(t, db, 9101, "econ_silver_item", 27)
	seedEconomyItemDef(t, db, 9102, "econ_gold_item", 27)
	if _, err := db.SQL.Exec(`INSERT INTO shop_item(store_id,item_id,silver,gold,stock) VALUES
		('store','econ_silver_item',100,0,10),('store','econ_gold_item',0,100,10)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnsureProfile(context.Background(), uid); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`UPDATE profile SET silver=1000,gold=1000 WHERE uid=?`, uid); err != nil {
		t.Fatal(err)
	}

	wrongGold := economyTestCtx(srv, uid, proto.Args524(
		proto.StrVal("store"), proto.StrVal("econ_silver_item"), proto.U64(0)))
	wrongGold.ExpectResponse = true
	wrongGold.replyCapture = &ctxReplyCapture{}
	hStoreBuy(true)(wrongGold)
	if _, _, _, failure := wrongGold.replyCapture.snapshot(); !failure {
		t.Fatal("银币-only 商品必须通过原生错误拒绝金币购买")
	}
	wrongSilver := economyTestCtx(srv, uid, proto.Args524(
		proto.StrVal("store"), proto.StrVal("econ_gold_item"), proto.U64(0)))
	wrongSilver.ExpectResponse = true
	wrongSilver.replyCapture = &ctxReplyCapture{}
	hStoreBuy(false)(wrongSilver)
	if _, _, _, failure := wrongSilver.replyCapture.snapshot(); !failure {
		t.Fatal("金币-only 商品必须通过原生错误拒绝银币购买")
	}
	okSilver := economyTestCtx(srv, uid, proto.Args524(
		proto.StrVal("store"), proto.StrVal("econ_silver_item"), proto.U64(0)))
	if body, _ := hStoreBuy(false)(okSilver); len(body) == 0 || body[0] == 0x0F {
		t.Fatalf("银币购买应成功: %x", body)
	}
	okGold := economyTestCtx(srv, uid, proto.Args524(
		proto.StrVal("store"), proto.StrVal("econ_gold_item"), proto.U64(0)))
	if body, _ := hStoreBuy(true)(okGold); len(body) == 0 || body[0] == 0x0F {
		t.Fatalf("金币购买应成功: %x", body)
	}
	var silver, gold, txns, items int64
	if err := db.SQL.QueryRow(`SELECT silver,gold FROM profile WHERE uid=?`, uid).Scan(&silver, &gold); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM txn WHERE uid=? AND kind='buy'`, uid).Scan(&txns); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM item WHERE uid=?`, uid).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if silver != 900 || gold != 900 || txns != 2 || items != 2 {
		t.Fatalf("购买账实不符: silver=%d gold=%d txn=%d items=%d", silver, gold, txns, items)
	}
}

func TestCraftMissingMaterialAndRollback(t *testing.T) {
	srv, db := newEconomyTxTestServer(t)
	uid := int64(2003)
	seedEconomyItemDef(t, db, 9201, "econ_mat", 27)
	seedEconomyItemDef(t, db, 9202, "econ_out", 27)
	materials := `[{"demand_id":"econ_req","item_id":"econ_mat","qty":2}]`
	if _, err := db.SQL.Exec(`INSERT INTO recipe(recipe_id,name,output_id,output_qty,materials_json,duration_sec,kind)
		VALUES('econ_recipe','测试配方','econ_out',1,?,0,'craft')`, materials); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnsureProfile(context.Background(), uid); err != nil {
		t.Fatal(err)
	}

	missing := economyTestCtx(srv, uid, economyCraftTree("econ_recipe", nil, 1))
	if body, ok := hCraft(missing); !ok || len(body) == 0 {
		t.Fatalf("9/1 材料不足应有 803 响应: ok=%v body=%x", ok, body)
	}
	var n int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM item WHERE uid=? AND item_id='econ_out'`, uid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("缺料不得产出，out=%d", n)
	}

	economyTestItem(t, srv, uid, "econ_mat", "backpack", 2)
	okCtx := economyTestCtx(srv, uid, economyCraftTree("econ_recipe", nil, 1))
	if body, ok := hCraft(okCtx); !ok || len(body) == 0 {
		t.Fatalf("9/1 材料充足应成功: ok=%v body=%x", ok, body)
	}
	var matCount int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM item WHERE uid=? AND item_id='econ_mat'`, uid).Scan(&matCount); err != nil {
		t.Fatal(err)
	}
	if matCount != 0 {
		t.Fatalf("材料应被扣完，mat=%d", matCount)
	}

	economyTestItem(t, srv, uid, "econ_mat", "backpack", 2)
	if _, err := db.SQL.Exec(`CREATE TRIGGER econ_fail_output BEFORE INSERT ON item
		WHEN NEW.item_id='econ_out' BEGIN SELECT RAISE(ABORT,'boom'); END`); err != nil {
		t.Fatal(err)
	}
	// 产出可计件时走并入已有堆的 UPDATE 路径，同样要触发失败以验证回滚。
	if _, err := db.SQL.Exec(`CREATE TRIGGER econ_fail_output_upd BEFORE UPDATE ON item
		WHEN NEW.item_id='econ_out' BEGIN SELECT RAISE(ABORT,'boom'); END`); err != nil {
		t.Fatal(err)
	}
	var outBefore int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM item WHERE uid=? AND item_id='econ_out'`, uid).Scan(&outBefore); err != nil {
		t.Fatal(err)
	}
	failCtx := economyTestCtx(srv, uid, economyCraftTree("econ_recipe", nil, 1))
	if body, ok := hCraft(failCtx); !ok || len(body) == 0 {
		t.Fatalf("9/1 产出失败仍应回 803: ok=%v body=%x", ok, body)
	}
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM item WHERE uid=? AND item_id='econ_mat'`, uid).Scan(&matCount); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM item WHERE uid=? AND item_id='econ_out'`, uid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if matCount != 1 || n != outBefore {
		t.Fatalf("产出失败必须回滚扣料: mat=%d out_before=%d out_after=%d", matCount, outBefore, n)
	}
}

func TestMailSendAcceptAtomic(t *testing.T) {
	srv, db := newEconomyTxTestServer(t)
	sender, recipient := int64(2004), int64(2005)
	seedEconomyItemDef(t, db, 9301, "econ_mail_item", 27)
	seedEconomyItemDef(t, db, 9302, "econ_mail_a", 27)
	seedEconomyItemDef(t, db, 9303, "econ_mail_b", 27)
	for _, uid := range []int64{sender, recipient} {
		if _, err := db.EnsureProfile(context.Background(), uid); err != nil {
			t.Fatal(err)
		}
	}
	item := economyTestItem(t, srv, sender, "econ_mail_item", "backpack", 3)
	sendCtx := economyTestCtx(srv, sender, proto.Args524(
		proto.U32(uint32(recipient)), proto.U32(2), proto.Guid(item.GUID)))
	if body, ok := hMailSendItem(sendCtx); !ok || len(body) == 0 || body[0] == 0x0F {
		t.Fatalf("19/2 寄件应成功: ok=%v body=%x", ok, body)
	}
	left, err := db.GetItemByGUID(context.Background(), item.GUID)
	if err != nil || left == nil || left.Qty != 1 {
		t.Fatalf("寄件后应剩 1: item=%+v err=%v", left, err)
	}
	var mailID int64
	var raw string
	if err := db.SQL.QueryRow(`SELECT id,items_json FROM mail WHERE uid=? AND sender=?`, recipient, sender).Scan(&mailID, &raw); err != nil {
		t.Fatal(err)
	}
	var atts []store.MailItem
	if err := json.Unmarshal([]byte(raw), &atts); err != nil {
		t.Fatal(err)
	}
	if len(atts) != 1 || atts[0].Qty != 2 || len(mailIDToGUID(mailID)) != 32 {
		t.Fatalf("附件/邮件 GUID 不符: id=%d guid=%q atts=%+v", mailID, mailIDToGUID(mailID), atts)
	}
	acceptCtx := economyTestCtx(srv, recipient, proto.Args524(proto.Guid(mailIDToGUID(mailID))))
	if body, ok := hMailAccept(acceptCtx); !ok || len(body) != 1 {
		t.Fatalf("19/3 领取应回 null: ok=%v body=%x", ok, body)
	}
	var receivedGUID string
	if err := db.SQL.QueryRow(`SELECT guid FROM item WHERE uid=? AND item_id='econ_mail_item'`, recipient).Scan(&receivedGUID); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetItemByGUID(context.Background(), receivedGUID)
	if err != nil || got == nil || got.UID != recipient || got.Qty != 2 {
		t.Fatalf("领取附件数量不符: item=%+v err=%v", got, err)
	}
	var read int
	if err := db.SQL.QueryRow(`SELECT read FROM mail WHERE id=?`, mailID).Scan(&read); err != nil || read != 1 {
		t.Fatalf("领取后邮件应已读: read=%d err=%v", read, err)
	}

	badJSON, _ := json.Marshal([]store.MailItem{
		{ItemID: "econ_mail_a", Qty: 1},
		{ItemID: "econ_mail_b", Qty: 1},
	})
	res, err := db.SQL.Exec(`INSERT INTO mail(uid,sender,kind,text,items_json,created_ts,read)
		VALUES(?,?,1,'',?,0,0)`, recipient, sender, string(badJSON))
	if err != nil {
		t.Fatal(err)
	}
	badID, _ := res.LastInsertId()
	if _, err := db.SQL.Exec(`CREATE TRIGGER econ_fail_mail BEFORE INSERT ON item
		WHEN NEW.item_id='econ_mail_b' BEGIN SELECT RAISE(ABORT,'boom'); END`); err != nil {
		t.Fatal(err)
	}
	badCtx := economyTestCtx(srv, recipient, proto.Args524(proto.Guid(mailIDToGUID(badID))))
	_, _ = hMailAccept(badCtx)
	var aCount int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM item WHERE uid=? AND item_id IN ('econ_mail_a','econ_mail_b')`,
		recipient).Scan(&aCount); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT read FROM mail WHERE id=?`, badID).Scan(&read); err != nil {
		t.Fatal(err)
	}
	if aCount != 0 || read != 0 {
		t.Fatalf("领取中途失败必须整批回滚: items=%d read=%d", aCount, read)
	}
}

func TestPlaceBuyoutAtomicAndIdempotent(t *testing.T) {
	srv, db := newEconomyTxTestServer(t)
	uid := int64(2006)
	place := "level_010_belaya"
	tier := placeBuyoutPrice(place, 1)
	if tier <= 0 {
		t.Fatalf("测试地点买断价未配置: %s", place)
	}
	if _, err := db.EnsureProfile(context.Background(), uid); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`UPDATE profile SET gold=? WHERE uid=?`, tier*2, uid); err != nil {
		t.Fatal(err)
	}
	tree := proto.Args524(proto.StrVal(place), proto.U32(1), proto.U64(uint64(tier)))
	body, ok := hPlaceBuyoutTx(economyTestCtx(srv, uid, tree))
	if !ok || len(body) == 0 {
		t.Fatalf("3/37 买断应回 307: ok=%v body=%x", ok, body)
	}
	var gold int64
	if err := db.SQL.QueryRow(`SELECT gold FROM profile WHERE uid=?`, uid).Scan(&gold); err != nil {
		t.Fatal(err)
	}
	if gold != tier {
		t.Fatalf("买断金币不符: gold=%d want=%d", gold, tier)
	}
	body, ok = hPlaceBuyoutTx(economyTestCtx(srv, uid, tree))
	if !ok || len(body) == 0 {
		t.Fatalf("重复买断仍应回档案: ok=%v body=%x", ok, body)
	}
	var txnCount int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM txn WHERE uid=? AND kind='buy' AND item_id=?`, uid, place).Scan(&txnCount); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT gold FROM profile WHERE uid=?`, uid).Scan(&gold); err != nil {
		t.Fatal(err)
	}
	if gold != tier || txnCount != 1 {
		t.Fatalf("重复买断不得二次扣款: gold=%d txn=%d", gold, txnCount)
	}
	if _, err := db.SQL.Exec(`DELETE FROM place_access WHERE uid=? AND place=?`, uid, place); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`UPDATE profile SET gold=? WHERE uid=?`, tier-1, uid); err != nil {
		t.Fatal(err)
	}
	body, ok = hPlaceBuyoutTx(economyTestCtx(srv, uid, tree))
	if !ok || len(body) == 0 {
		t.Fatalf("余额不足仍应回档案: ok=%v body=%x", ok, body)
	}
	var owned int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM place_access WHERE uid=? AND place=?`, uid); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT gold FROM profile WHERE uid=?`, uid).Scan(&gold); err != nil {
		t.Fatal(err)
	}
	if owned != 0 || gold != tier-1 {
		t.Fatalf("余额不足不得授权/扣款: owned=%d gold=%d", gold)
	}
}

// 纸质地图只是可阅读道具：购买后扣钱、入库，但不写入 place_access，
// 否则"买张地图"会变成该地点的永久买断（无限访问）。地点买断只走 3/37。
func TestPaperMapBuyDoesNotUnlockPlace(t *testing.T) {
	srv, db := newEconomyTxTestServer(t)
	uid := int64(2007)
	if _, err := db.SQL.Exec(`INSERT INTO shop_item(store_id,item_id,silver,gold,stock) VALUES
		('admin','papermap_torfynoe_01',100,0,10),
		('admin','papermap_belaya_01',100,0,10)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnsureProfile(context.Background(), uid); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`UPDATE profile SET silver=1000 WHERE uid=?`, uid); err != nil {
		t.Fatal(err)
	}
	buy := func(itemID, place string) {
		t.Helper()
		body, ok := hStoreBuy(false)(economyTestCtx(srv, uid, proto.Args524(
			proto.StrVal("admin"), proto.StrVal(itemID), proto.U64(0))))
		if !ok || len(body) == 0 || body[0] == 0x0F {
			t.Fatalf("购买 %s 失败: ok=%v body=%x", itemID, ok, body)
		}
		var owned int
		if err := db.SQL.QueryRow(
			`SELECT COUNT(*) FROM place_access WHERE uid=? AND place=?`,
			uid, place).Scan(&owned); err != nil {
			t.Fatal(err)
		}
		if owned != 0 {
			t.Fatalf("%s 不应写入 place_access（%s），实得 %d 行", itemID, place, owned)
		}
	}
	buy("papermap_torfynoe_01", "level_004_torfyanoe_ozero")
	buy("papermap_belaya_01", "level_010_belaya")
	var silver int64
	if err := db.SQL.QueryRow(`SELECT silver FROM profile WHERE uid=?`, uid).Scan(&silver); err != nil {
		t.Fatal(err)
	}
	if silver != 800 {
		t.Fatalf("两张地图应各扣 100，silver=%d", silver)
	}
}

func TestMarketPriceUsesMapOverlay(t *testing.T) {
	// 找一条在目标地图缺数据的鱼，验证跨图时按 marketMapPct 水域倍率折算。
	// 基准图选择必须与 ruPricePerKg 完全一致：排序后第一张有该鱼数据且倍率有效的图。
	r := loadRUPriceRules()
	if r == nil || len(r.ByMap) < 2 {
		t.Skip("价格表地图不足")
	}
	weight := int64(1000)
	for _, targetMap := range ruSortedMapKeys(r) {
		targetPct := marketMapPct(targetMap)
		if targetPct <= 0 {
			continue
		}
		for _, candidateBase := range ruSortedMapKeys(r) {
			if candidateBase == targetMap {
				continue
			}
			if marketMapPct(candidateBase) <= 0 {
				continue
			}
			for fishKey, rec := range r.ByMap[candidateBase] {
				if rec["1"] <= 0 && rec["2"] <= 0 {
					continue
				}
				// 目标图必须缺该鱼数据。
				if r.tablePricePerKg(targetMap, fishKey, 2) > 0 {
					continue
				}
				// 实现里真正使用的基准图：排序后第一张有数据且倍率有效者。
				var implBase string
				for _, bm := range ruSortedMapKeys(r) {
					if bm == targetMap {
						continue
					}
					if r.tablePricePerKg(bm, fishKey, 2) > 0 && marketMapPct(bm) > 0 {
						implBase = bm
						break
					}
				}
				if implBase == "" {
					continue
				}
				basePct := marketMapPct(implBase)
				if basePct == targetPct {
					continue
				}
				e := &store.KeepnetEntry{Species: fishKey, WeightG: weight, Cat: 2, TS: 1}
				scaled := marketPriceCentsForMap(e, 1, targetMap)
				wantScaled := int64(r.tablePricePerKg(implBase, fishKey, 2)/basePct*targetPct*float64(weight)/1000.0*100.0 + 0.5)
				// 允许 1 分误差：浮点乘除取整顺序在 .5 边界可能相差 1 分。
				if scaled > wantScaled+1 || scaled < wantScaled-1 {
					t.Fatalf("地图倍率折算不符: scaled=%d want=%d(%s->%s, %s)", scaled, wantScaled, implBase, targetMap, fishKey)
				}
				return
			}
		}
	}
	t.Skip("没有可验证跨图折算的鱼种")
}
