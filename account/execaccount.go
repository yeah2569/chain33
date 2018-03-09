package account

import (
	"code.aliyun.com/chain33/chain33/queue"
	"code.aliyun.com/chain33/chain33/types"
)

func (acc *AccountDB) LoadExecAccount(addr, execaddr string) *types.Account {
	value, err := acc.db.Get(acc.ExecKey(addr, execaddr))
	if err != nil {
		return &types.Account{Addr: addr}
	}
	var acc1 types.Account
	err = types.Decode(value, &acc1)
	if err != nil {
		panic(err) //数据库已经损坏
	}
	return &acc1
}

func (acc *AccountDB) LoadExecAccountQueue(client queue.Client, addr, execaddr string) (*types.Account, error) {
	msg := client.NewMessage("blockchain", types.EventGetLastHeader, nil)
	client.Send(msg, true)
	msg, err := client.Wait(msg)
	if err != nil {
		return nil, err
	}
	get := types.StoreGet{}
	get.StateHash = msg.GetData().(*types.Header).GetStateHash()
	get.Keys = append(get.Keys, acc.ExecKey(addr, execaddr))
	msg = client.NewMessage("store", types.EventStoreGet, &get)
	client.Send(msg, true)
	msg, err = client.Wait(msg)
	if err != nil {
		return nil, err
	}
	values := msg.GetData().(*types.StoreReplyValue)
	value := values.Values[0]
	if value == nil {
		return &types.Account{Addr: addr}, nil
	} else {
		var acc1 types.Account
		err := types.Decode(value, &acc1)
		if err != nil {
			return nil, err
		}
		return &acc1, nil
	}
}

func (acc *AccountDB) SaveExecAccount(execaddr string, acc1 *types.Account) {
	set := acc.GetExecKVSet(execaddr, acc1)
	for i := 0; i < len(set); i++ {
		acc.db.Set(set[i].GetKey(), set[i].Value)
	}
}

func (acc *AccountDB) GetExecKVSet(execaddr string, acc1 *types.Account) (kvset []*types.KeyValue) {
	value := types.Encode(acc1)
	kvset = append(kvset, &types.KeyValue{acc.ExecKey(acc1.Addr, execaddr), value})
	return kvset
}

func (acc *AccountDB) ExecKey(address, execaddr string) (key []byte) {
	key = append(key, acc.execAccountKeyPerfix...)
	key = append(key, []byte(execaddr)...)
	key = append(key, []byte(":")...)
	key = append(key, []byte(address)...)
	return key
}

func (acc *AccountDB) TransferToExec(from, to string, amount int64) (*types.Receipt, error) {
	receipt, err := acc.Transfer(from, to, amount)
	if err != nil {
		return nil, err
	}
	receipt2, err := acc.execDeposit(from, to, amount)
	if err != nil {
		//存款不应该出任何问题
		panic(err)
	}
	return acc.mergeReceipt(receipt, receipt2), nil
}

func (acc *AccountDB) TransferWithdraw(from, to string, amount int64) (*types.Receipt, error) {
	//先判断可以取款
	if err := acc.CheckTransfer(to, from, amount); err != nil {
		return nil, err
	}
	receipt, err := acc.execWithdraw(to, from, amount)
	if err != nil {
		return nil, err
	}
	//然后执行transfer
	receipt2, err := acc.Transfer(to, from, amount)
	if err != nil {
		panic(err) //在withdraw
	}
	return acc.mergeReceipt(receipt, receipt2), nil
}

//四个操作中 Deposit 自动完成，不需要模块外的函数来调用
func (acc *AccountDB) ExecFrozen(addr, execaddr string, amount int64) (*types.Receipt, error) {
	if addr == execaddr {
		return nil, types.ErrSendSameToRecv
	}
	if !types.CheckAmount(amount) {
		return nil, types.ErrAmount
	}
	acc1 := acc.LoadExecAccount(addr, execaddr)
	if acc1.Balance-amount < 0 {
		alog.Error("ExecFrozen", "balance", acc1.Balance, "amount", amount)
		return nil, types.ErrNoBalance
	}
	copyacc := *acc1
	acc1.Balance -= amount
	acc1.Frozen += amount
	receiptBalance := &types.ReceiptExecAccountTransfer{execaddr, &copyacc, acc1}
	acc.SaveExecAccount(execaddr, acc1)
	return acc.execReceipt(types.TyLogExecFrozen, acc1, receiptBalance), nil
}

func (acc *AccountDB) ExecActive(addr, execaddr string, amount int64) (*types.Receipt, error) {
	if addr == execaddr {
		return nil, types.ErrSendSameToRecv
	}
	if !types.CheckAmount(amount) {
		return nil, types.ErrAmount
	}
	acc1 := acc.LoadExecAccount(addr, execaddr)
	if acc1.Frozen-amount < 0 {
		return nil, types.ErrNoBalance
	}
	copyacc := *acc1
	acc1.Balance += amount
	acc1.Frozen -= amount
	receiptBalance := &types.ReceiptExecAccountTransfer{execaddr, &copyacc, acc1}
	acc.SaveExecAccount(execaddr, acc1)
	return acc.execReceipt(types.TyLogExecActive, acc1, receiptBalance), nil
}

func (acc *AccountDB) ExecTransfer(from, to, execaddr string, amount int64) (*types.Receipt, error) {
	if from == to {
		return nil, types.ErrSendSameToRecv
	}
	if !types.CheckAmount(amount) {
		return nil, types.ErrAmount
	}
	accFrom := acc.LoadExecAccount(from, execaddr)
	accTo := acc.LoadExecAccount(to, execaddr)

	if accFrom.GetBalance()-amount < 0 {
		return nil, types.ErrNoBalance
	}
	copyaccFrom := *accFrom
	copyaccTo := *accTo

	accFrom.Balance -= amount
	accTo.Balance += amount

	receiptBalanceFrom := &types.ReceiptExecAccountTransfer{execaddr, &copyaccFrom, accFrom}
	receiptBalanceTo := &types.ReceiptExecAccountTransfer{execaddr, &copyaccTo, accTo}

	acc.SaveExecAccount(execaddr, accFrom)
	acc.SaveExecAccount(execaddr, accTo)
	return acc.execReceipt2(accFrom, accTo, receiptBalanceFrom, receiptBalanceTo), nil
}

//从自己冻结的钱里面扣除，转移到别人的活动钱包里面去
func (acc *AccountDB) ExecTransferFrozen(from, to, execaddr string, amount int64) (*types.Receipt, error) {
	if from == to {
		return nil, types.ErrSendSameToRecv
	}
	if !types.CheckAmount(amount) {
		return nil, types.ErrAmount
	}
	accFrom := acc.LoadExecAccount(from, execaddr)
	accTo := acc.LoadExecAccount(to, execaddr)
	b := accFrom.GetFrozen() - amount
	if b < 0 {
		return nil, types.ErrNoBalance
	}
	copyaccFrom := *accFrom
	copyaccTo := *accTo

	accFrom.Frozen -= amount
	accTo.Balance += amount

	receiptBalanceFrom := &types.ReceiptExecAccountTransfer{execaddr, &copyaccFrom, accFrom}
	receiptBalanceTo := &types.ReceiptExecAccountTransfer{execaddr, &copyaccTo, accTo}

	acc.SaveExecAccount(execaddr, accFrom)
	acc.SaveExecAccount(execaddr, accTo)
	return acc.execReceipt2(accFrom, accTo, receiptBalanceFrom, receiptBalanceTo), nil
}

func (acc *AccountDB) ExecAddress(name string) *Address {
	return ExecAddress(name)
}

func (acc *AccountDB) ExecDepositFrozen(addr, execaddr string, amount int64) (*types.Receipt, error) {
	if addr == execaddr {
		return nil, types.ErrSendSameToRecv
	}
	//这个函数只有挖矿的合约才能调用
	list := types.AllowDepositExec
	allow := false
	for _, exec := range list {
		if acc.ExecAddress(exec).String() == execaddr {
			allow = true
			break
		}
	}
	if !allow {
		return nil, types.ErrNotAllowDeposit
	}
	receipt1, err := acc.depositBalance(execaddr, amount)
	if err != nil {
		return nil, err
	}
	receipt2, err := acc.execDepositFrozen(addr, execaddr, amount)
	if err != nil {
		return nil, err
	}
	return acc.mergeReceipt(receipt1, receipt2), nil
}

func (acc *AccountDB) execDepositFrozen(addr, execaddr string, amount int64) (*types.Receipt, error) {
	if addr == execaddr {
		return nil, types.ErrSendSameToRecv
	}
	if !types.CheckAmount(amount) {
		return nil, types.ErrAmount
	}
	acc1 := acc.LoadExecAccount(addr, execaddr)
	copyacc := *acc1
	acc1.Frozen += amount
	receiptBalance := &types.ReceiptExecAccountTransfer{execaddr, &copyacc, acc1}
	acc.SaveExecAccount(execaddr, acc1)
	return acc.execReceipt(types.TyLogExecDeposit, acc1, receiptBalance), nil
}

func (acc *AccountDB) execDeposit(addr, execaddr string, amount int64) (*types.Receipt, error) {
	if addr == execaddr {
		return nil, types.ErrSendSameToRecv
	}
	if !types.CheckAmount(amount) {
		return nil, types.ErrAmount
	}
	acc1 := acc.LoadExecAccount(addr, execaddr)
	copyacc := *acc1
	acc1.Balance += amount
	receiptBalance := &types.ReceiptExecAccountTransfer{execaddr, &copyacc, acc1}
	//alog.Debug("execDeposit", "addr", addr, "execaddr", execaddr, "account", acc)
	acc.SaveExecAccount(execaddr, acc1)
	return acc.execReceipt(types.TyLogExecDeposit, acc1, receiptBalance), nil
}

func (acc *AccountDB) execWithdraw(execaddr, addr string, amount int64) (*types.Receipt, error) {
	if addr == execaddr {
		return nil, types.ErrSendSameToRecv
	}
	if !types.CheckAmount(amount) {
		return nil, types.ErrAmount
	}
	acc1 := acc.LoadExecAccount(addr, execaddr)
	if acc1.Balance-amount < 0 {
		return nil, types.ErrNoBalance
	}
	copyacc := *acc1
	acc1.Balance -= amount
	receiptBalance := &types.ReceiptExecAccountTransfer{execaddr, &copyacc, acc1}
	acc.SaveExecAccount(execaddr, acc1)
	return acc.execReceipt(types.TyLogExecWithdraw, acc1, receiptBalance), nil
}

func (acc *AccountDB) execReceipt(ty int32, acc1 *types.Account, r *types.ReceiptExecAccountTransfer) *types.Receipt {
	log1 := &types.ReceiptLog{ty, types.Encode(r)}
	kv := acc.GetExecKVSet(r.ExecAddr, acc1)
	return &types.Receipt{types.ExecOk, kv, []*types.ReceiptLog{log1}}
}

func (acc *AccountDB) execReceipt2(acc1, acc2 *types.Account, r1, r2 *types.ReceiptExecAccountTransfer) *types.Receipt {
	log1 := &types.ReceiptLog{types.TyLogExecTransfer, types.Encode(r1)}
	log2 := &types.ReceiptLog{types.TyLogExecTransfer, types.Encode(r2)}
	kv := acc.GetExecKVSet(r1.ExecAddr, acc1)
	kv = append(kv, acc.GetExecKVSet(r2.ExecAddr, acc2)...)
	return &types.Receipt{types.ExecOk, kv, []*types.ReceiptLog{log1, log2}}
}

func (acc *AccountDB) mergeReceipt(receipt, receipt2 *types.Receipt) *types.Receipt {
	receipt.Logs = append(receipt.Logs, receipt2.Logs...)
	receipt.KV = append(receipt.KV, receipt2.KV...)
	return receipt
}
