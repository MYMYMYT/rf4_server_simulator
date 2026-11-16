package gameserver

// 25.f24（bapnijnooje）物品状态位。鱼护格与鱼市卖鱼列表这类物品卡按「稀有」位
// 渲染紫色稀有标；结算卡与图鉴的紫/粉分级另走 25.f4（jmpghlofkoc）。
const (
	keepnetStateRare     uint32 = 1 << 0  // 1=复古/稀有
	keepnetStateNameable uint32 = 1 << 22 // 蓝冠未命名可命名位
)

// keepnetStateBits 汇总 25.f24 状态位：稀有鱼种置稀有位，蓝冠未命名保留可命名位。
func keepnetStateBits(grade int, nameZh string, rarity int64) uint32 {
	var state uint32
	if rarity > 0 {
		state |= keepnetStateRare
	}
	if grade >= 4 && nameZh == "" {
		state |= keepnetStateNameable
	}
	return state
}
