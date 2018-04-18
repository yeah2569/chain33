package wallet

import (
	"encoding/hex"
	"errors"
	"sync/atomic"
	"time"

	"gitlab.33.cn/chain33/chain33/account"
	"gitlab.33.cn/chain33/chain33/common/crypto"
	"gitlab.33.cn/chain33/chain33/types"
	"gitlab.33.cn/chain33/chain33/common/crypto/privacy"
	"unsafe"
	"gitlab.33.cn/chain33/chain33/common"
)

func (wallet *Wallet) openticket(mineraddr, returnaddr string, priv crypto.PrivKey, count int32) ([]byte, error) {
	walletlog.Info("openticket", "mineraddr", mineraddr, "returnaddr", returnaddr, "count", int32(count))
	ta := &types.TicketAction{}
	topen := &types.TicketOpen{MinerAddress: mineraddr, ReturnAddress: returnaddr, Count: count}
	ta.Value = &types.TicketAction_Topen{topen}
	ta.Ty = types.TicketActionOpen
	return wallet.sendTransaction(ta, []byte("ticket"), priv, "")
}

func (wallet *Wallet) bindminer(mineraddr, returnaddr string, priv crypto.PrivKey) ([]byte, error) {
	ta := &types.TicketAction{}
	tbind := &types.TicketBind{MinerAddress: mineraddr, ReturnAddress: returnaddr}
	ta.Value = &types.TicketAction_Tbind{tbind}
	ta.Ty = types.TicketActionBind
	return wallet.sendTransaction(ta, []byte("ticket"), priv, "")
}

//通过rpc 精选close 操作
func (wallet *Wallet) closeTickets(priv crypto.PrivKey, ids []string) ([]byte, error) {
	//每次最多close 200个
	end := 200
	if end > len(ids) {
		end = len(ids)
	}
	walletlog.Info("closeTickets", "ids", ids[0:end])
	ta := &types.TicketAction{}
	tclose := &types.TicketClose{ids[0:end]}
	ta.Value = &types.TicketAction_Tclose{tclose}
	ta.Ty = types.TicketActionClose
	return wallet.sendTransaction(ta, []byte("ticket"), priv, "")
}

func (wallet *Wallet) getBalance(addr string, execer string) (*types.Account, error) {
	reqbalance := &types.ReqBalance{Addresses: []string{addr}, Execer: execer}
	reply, err := wallet.queryBalance(reqbalance)
	if err != nil {
		return nil, err
	}
	return reply[0], nil
}

func (wallet *Wallet) GetTickets(status int32) ([]*types.Ticket, [][]byte, error) {
	accounts, err := wallet.ProcGetAccountList()
	if err != nil {
		return nil, nil, err
	}
	wallet.mtx.Lock()
	defer wallet.mtx.Unlock()
	ok, err := wallet.CheckWalletStatus()
	if !ok && err != types.ErrOnlyTicketUnLocked {
		return nil, nil, err
	}
	//循环遍历所有的账户-->保证钱包已经解锁
	var tickets []*types.Ticket
	var privs [][]byte
	for _, account := range accounts.Wallets {
		t, err := wallet.getTickets(account.Acc.Addr, status)
		if err != nil {
			return nil, nil, err
		}
		if t != nil {
			priv, err := wallet.getPrivKeyByAddr(account.Acc.Addr)
			if err != nil {
				return nil, nil, err
			}
			privs = append(privs, priv.Bytes())
			tickets = append(tickets, t...)
		}
	}
	if len(tickets) == 0 {
		return nil, nil, types.ErrNoTicket
	}
	return tickets, privs, nil
}

func (wallet *Wallet) getAllPrivKeys() ([]crypto.PrivKey, error) {
	accounts, err := wallet.ProcGetAccountList()
	if err != nil {
		return nil, err
	}
	wallet.mtx.Lock()
	defer wallet.mtx.Unlock()
	ok, err := wallet.CheckWalletStatus()
	if !ok && err != types.ErrOnlyTicketUnLocked {
		return nil, err
	}
	var privs []crypto.PrivKey
	for _, account := range accounts.Wallets {
		priv, err := wallet.getPrivKeyByAddr(account.Acc.Addr)
		if err != nil {
			return nil, err
		}
		privs = append(privs, priv)
	}
	return privs, nil
}

func (client *Wallet) GetHeight() int64 {
	msg := client.client.NewMessage("blockchain", types.EventGetBlockHeight, nil)
	client.client.Send(msg, true)
	replyHeight, err := client.client.Wait(msg)
	h := replyHeight.GetData().(*types.ReplyBlockHeight).Height
	walletlog.Debug("getheight = ", "height", h)
	if err != nil {
		return 0
	}
	return h
}

func (wallet *Wallet) closeAllTickets(height int64) (int, error) {
	keys, err := wallet.getAllPrivKeys()
	if err != nil {
		return 0, err
	}
	var hashes [][]byte
	for _, key := range keys {
		hash, err := wallet.closeTicketsByAddr(height, key)
		if err != nil {
			walletlog.Error("close Tickets By Addr", "err", err)
			continue
		}
		if hash == nil {
			continue
		}
		hashes = append(hashes, hash)
	}
	if len(hashes) > 0 {
		wallet.waitTxs(hashes)
		return len(hashes), nil
	}
	return 0, nil
}

func (wallet *Wallet) forceCloseAllTicket(height int64) (*types.ReplyHashes, error) {
	keys, err := wallet.getAllPrivKeys()
	if err != nil {
		return nil, err
	}
	var hashes types.ReplyHashes
	for _, key := range keys {
		hash, err := wallet.forceCloseTicketsByAddr(height, key)
		if err != nil {
			walletlog.Error("close Tickets By Addr", "err", err)
			continue
		}
		if hash == nil {
			continue
		}
		hashes.Hashes = append(hashes.Hashes, hash)
	}
	return &hashes, nil
}

func (wallet *Wallet) withdrawFromTicketOne(priv crypto.PrivKey) ([]byte, error) {
	addr := account.PubKeyToAddress(priv.PubKey().Bytes()).String()
	acc, err := wallet.getBalance(addr, "ticket")
	if err != nil {
		return nil, err
	}
	if acc.Balance > 0 {
		hash, err := wallet.sendToAddress(priv, account.ExecAddress("ticket").String(), -acc.Balance, "autominer->withdraw", false, "")

		if err != nil {
			return nil, err
		}
		return hash.GetHash(), nil
	}
	return nil, nil
}

func (wallet *Wallet) buyTicketOne(height int64, priv crypto.PrivKey) ([]byte, int, error) {
	//ticket balance and coins balance
	addr := account.PubKeyToAddress(priv.PubKey().Bytes()).String()
	acc1, err := wallet.getBalance(addr, "coins")
	if err != nil {
		return nil, 0, err
	}
	acc2, err := wallet.getBalance(addr, "ticket")
	if err != nil {
		return nil, 0, err
	}
	//留一个币作为手续费，如果手续费不够了，不能挖矿
	//判断手续费是否足够，如果不足要及时补充。
	fee := types.Coin
	if acc1.Balance+acc2.Balance-2*fee >= types.GetP(height).TicketPrice {
		//第一步。转移币到 ticket
		toaddr := account.ExecAddress("ticket").String()
		amount := acc1.Balance - 2*fee
		//必须大于0，才需要转移币
		var hash *types.ReplyHash
		if amount > 0 {
			walletlog.Info("buyTicketOne.send", "toaddr", toaddr, "amount", amount)
			hash, err = wallet.sendToAddress(priv, toaddr, amount, "coins->ticket", false, "")

			if err != nil {
				return nil, 0, err
			}
			wallet.waitTx(hash.Hash)
		}
		acc, err := wallet.getBalance(addr, "ticket")
		if err != nil {
			return nil, 0, err
		}
		count := acc.Balance / types.GetP(height).TicketPrice
		if count > 0 {
			txhash, err := wallet.openticket(addr, addr, priv, int32(count))
			return txhash, int(count), err
		}
	}
	return nil, 0, nil
}

func (wallet *Wallet) buyMinerAddrTicketOne(height int64, priv crypto.PrivKey) ([][]byte, int, error) {
	addr := account.PubKeyToAddress(priv.PubKey().Bytes()).String()
	//判断是否绑定了coldaddr
	addrs, err := wallet.getMinerColdAddr(addr)
	if err != nil {
		return nil, 0, err
	}
	total := 0
	var hashes [][]byte
	for i := 0; i < len(addrs); i++ {
		walletlog.Info("sourceaddr", "addr", addrs[i])
		acc, err := wallet.getBalance(addrs[i], "ticket")
		if err != nil {
			return nil, 0, err
		}
		count := acc.Balance / types.GetP(height).TicketPrice
		if count > 0 {
			txhash, err := wallet.openticket(addr, addrs[i], priv, int32(count))
			if err != nil {
				return nil, 0, err
			}
			total += int(count)
			if txhash != nil {
				hashes = append(hashes, txhash)
			}
		}
	}
	return hashes, total, nil
}

func (wallet *Wallet) processFee(priv crypto.PrivKey) error {
	addr := account.PubKeyToAddress(priv.PubKey().Bytes()).String()
	acc1, err := wallet.getBalance(addr, "coins")
	if err != nil {
		return err
	}
	acc2, err := wallet.getBalance(addr, "ticket")
	if err != nil {
		return err
	}
	toaddr := account.ExecAddress("ticket").String()
	//如果acc2 的余额足够，那题withdraw 部分钱做手续费
	if (acc1.Balance < (types.Coin / 2)) && (acc2.Balance > types.Coin) {
		_, err := wallet.sendToAddress(priv, toaddr, -types.Coin, "ticket->coins", false, "")
		if err != nil {
			return err
		}
	}
	return nil
}

func (wallet *Wallet) closeTicketsByAddr(height int64, priv crypto.PrivKey) ([]byte, error) {
	wallet.processFee(priv)
	addr := account.PubKeyToAddress(priv.PubKey().Bytes()).String()
	tlist, err := wallet.getTickets(addr, 2)
	if err != nil && err != types.ErrNotFound {
		return nil, err
	}
	var ids []string
	var tl []*types.Ticket
	now := time.Now().Unix()
	for _, t := range tlist {
		if !t.IsGenesis {
			if now-t.GetCreateTime() < types.GetP(height).TicketWithdrawTime {
				continue
			}
			if now-t.GetMinerTime() < types.GetP(height).TicketMinerWaitTime {
				continue
			}
		}
		tl = append(tl, t)
	}
	for i := 0; i < len(tl); i++ {
		ids = append(ids, tl[i].TicketId)
	}
	if len(ids) > 0 {
		return wallet.closeTickets(priv, ids)
	}
	return nil, nil
}

func (wallet *Wallet) forceCloseTicketsByAddr(height int64, priv crypto.PrivKey) ([]byte, error) {
	wallet.processFee(priv)
	addr := account.PubKeyToAddress(priv.PubKey().Bytes()).String()
	tlist1, err1 := wallet.getTickets(addr, 1)
	if err1 != nil && err1 != types.ErrNotFound {
		return nil, err1
	}
	tlist2, err2 := wallet.getTickets(addr, 2)
	if err2 != nil && err2 != types.ErrNotFound {
		return nil, err1
	}
	tlist := append(tlist1, tlist2...)
	var ids []string
	var tl []*types.Ticket
	now := time.Now().Unix()
	for _, t := range tlist {
		if !t.IsGenesis {
			if t.Status == 1 && now-t.GetCreateTime() < types.GetP(height).TicketWithdrawTime {
				continue
			}
			if t.Status == 2 && now-t.GetCreateTime() < types.GetP(height).TicketWithdrawTime {
				continue
			}
			if t.Status == 2 && now-t.GetMinerTime() < types.GetP(height).TicketMinerWaitTime {
				continue
			}
		}
		tl = append(tl, t)
	}
	for i := 0; i < len(tl); i++ {
		ids = append(ids, tl[i].TicketId)
	}
	if len(ids) > 0 {
		return wallet.closeTickets(priv, ids)
	}
	return nil, nil
}

func (client *Wallet) getTickets(addr string, status int32) ([]*types.Ticket, error) {
	reqaddr := &types.TicketList{addr, status}
	var req types.Query
	req.Execer = []byte("ticket")
	req.FuncName = "TicketList"
	req.Payload = types.Encode(reqaddr)
	msg := client.client.NewMessage("blockchain", types.EventQuery, &req)
	client.client.Send(msg, true)
	resp, err := client.client.Wait(msg)
	if err != nil {
		return nil, err
	}
	reply := resp.GetData().(types.Message).(*types.ReplyTicketList)
	for i := 0; i < len(reply.Tickets); i++ {
		walletlog.Debug("Tickets", "id", reply.Tickets[i].GetTicketId(), "addr", addr, "req", status, "res", reply.Tickets[i].Status)
	}
	return reply.Tickets, nil
}

func (wallet *Wallet) sendTransactionWait(payload types.Message, execer []byte, priv crypto.PrivKey, to string) (err error) {
	hash, err := wallet.sendTransaction(payload, execer, priv, to)
	if err != nil {
		return err
	}
	txinfo := wallet.waitTx(hash)
	if txinfo.Receipt.Ty != types.ExecOk {
		return errors.New("sendTransactionWait error")
	}
	return nil
}

func (wallet *Wallet) sendTransaction(payload types.Message, execer []byte, priv crypto.PrivKey, to string) (hash []byte, err error) {
	if to == "" {
		to = account.ExecAddress(string(execer)).String()
	}
	tx := &types.Transaction{Execer: execer, Payload: types.Encode(payload), Fee: minFee, To: to}
	tx.Nonce = wallet.random.Int63()
	tx.Fee, err = tx.GetRealFee(wallet.getFee())
	if err != nil {
		return nil, err
	}
	tx.SetExpire(time.Second * 120)
	tx.Sign(int32(SignType), priv)
	reply, err := wallet.sendTx(tx)
	if err != nil {
		return nil, err
	}
	if !reply.IsOk {
		walletlog.Info("wallet sendTransaction", "err", string(reply.GetMsg()))
		return nil, errors.New(string(reply.GetMsg()))
	}
	return tx.Hash(), nil
}

func (wallet *Wallet) sendTx(tx *types.Transaction) (*types.Reply, error) {
	if wallet.client == nil {
		panic("client not bind message queue.")
	}
	msg := wallet.client.NewMessage("mempool", types.EventTx, tx)
	err := wallet.client.Send(msg, true)
	if err != nil {
		walletlog.Error("SendTx", "Error", err.Error())
		return nil, err
	}
	resp, err := wallet.client.Wait(msg)
	if err != nil {
		return nil, err
	}
	return resp.GetData().(*types.Reply), nil
}

func (wallet *Wallet) waitTx(hash []byte) *types.TransactionDetail {
	i := 0
	for {
		if atomic.LoadInt32(&wallet.isclosed) == 1 {
			return nil
		}
		i++
		if i%100 == 0 {
			walletlog.Error("wait transaction timeout", "hash", hex.EncodeToString(hash))
			return nil
		}
		res, err := wallet.queryTx(hash)
		if err != nil {
			time.Sleep(time.Second)
		}
		if res != nil {
			return res
		}
	}
}

func (wallet *Wallet) waitTxs(hashes [][]byte) (ret []*types.TransactionDetail) {
	for _, hash := range hashes {
		result := wallet.waitTx(hash)
		ret = append(ret, result)
	}
	return ret
}

func (client *Wallet) queryTx(hash []byte) (*types.TransactionDetail, error) {
	msg := client.client.NewMessage("blockchain", types.EventQueryTx, &types.ReqHash{hash})
	err := client.client.Send(msg, true)
	if err != nil {
		walletlog.Error("QueryTx", "Error", err.Error())
		return nil, err
	}
	resp, err := client.client.Wait(msg)
	if err != nil {
		return nil, err
	}
	return resp.Data.(*types.TransactionDetail), nil
}

func (wallet *Wallet) sendToAddress(priv crypto.PrivKey, addrto string, amount int64, note string, Istoken bool, tokenSymbol string) (*types.ReplyHash, error) {
	var tx *types.Transaction
	if !Istoken {
		transfer := &types.CoinsAction{}
		if amount > 0 {
			v := &types.CoinsAction_Transfer{&types.CoinsTransfer{Amount: amount, Note: note}}
			transfer.Value = v
			transfer.Ty = types.CoinsActionTransfer
		} else {
			v := &types.CoinsAction_Withdraw{&types.CoinsWithdraw{Amount: -amount, Note: note}}
			transfer.Value = v
			transfer.Ty = types.CoinsActionWithdraw
		}
		tx = &types.Transaction{Execer: []byte("coins"), Payload: types.Encode(transfer), Fee: wallet.getFee(), To: addrto, Nonce: wallet.random.Int63()}
	} else {
		transfer := &types.TokenAction{}
		if amount > 0 {
			v := &types.TokenAction_Transfer{&types.CoinsTransfer{Cointoken: tokenSymbol, Amount: amount, Note: note}}
			transfer.Value = v
			transfer.Ty = types.ActionTransfer
		} else {
			v := &types.TokenAction_Withdraw{&types.CoinsWithdraw{Cointoken: tokenSymbol, Amount: -amount, Note: note}}
			transfer.Value = v
			transfer.Ty = types.ActionWithdraw
		}
		tx = &types.Transaction{Execer: []byte("token"), Payload: types.Encode(transfer), Fee: wallet.getFee(), To: addrto, Nonce: wallet.random.Int63()}
	}
	tx.SetExpire(time.Second * 120)
	tx.Sign(int32(SignType), priv)

	//发送交易信息给mempool模块
	msg := wallet.client.NewMessage("mempool", types.EventTx, tx)
	wallet.client.Send(msg, true)
	resp, err := wallet.client.Wait(msg)
	if err != nil {
		walletlog.Error("ProcSendToAddress", "Send err", err)
		return nil, err
	}
	reply := resp.GetData().(*types.Reply)
	if !reply.GetIsOk() {
		return nil, errors.New(string(reply.GetMsg()))
	}
	var hash types.ReplyHash
	hash.Hash = tx.Hash()
	return &hash, nil
}

func (client *Wallet) queryBalance(in *types.ReqBalance) ([]*types.Account, error) {

	switch in.GetExecer() {
	case "coins":
		addrs := in.GetAddresses()
		var exaddrs []string
		for _, addr := range addrs {
			if err := account.CheckAddress(addr); err != nil {
				addr = account.ExecAddress(addr).String()
			}
			exaddrs = append(exaddrs, addr)
		}
		accounts, err := accountdb.LoadAccounts(client.client, exaddrs)
		if err != nil {
			walletlog.Error("GetBalance", "err", err.Error())
			return nil, err
		}
		return accounts, nil
	default:
		execaddress := account.ExecAddress(in.GetExecer())
		addrs := in.GetAddresses()
		var accounts []*types.Account
		for _, addr := range addrs {
			account, err := accountdb.LoadExecAccountQueue(client.client, addr, execaddress.String())
			if err != nil {
				walletlog.Error("GetBalance", "err", err.Error())
				return nil, err
			}
			accounts = append(accounts, account)
		}
		return accounts, nil
	}
	return nil, nil
}

func (client *Wallet) getMinerColdAddr(addr string) ([]string, error) {
	reqaddr := &types.ReqString{addr}
	var req types.Query
	req.Execer = []byte("ticket")
	req.FuncName = "MinerSourceList"
	req.Payload = types.Encode(reqaddr)

	msg := client.client.NewMessage("blockchain", types.EventQuery, &req)
	client.client.Send(msg, true)
	resp, err := client.client.Wait(msg)
	if err != nil {
		return nil, err
	}
	reply := resp.GetData().(types.Message).(*types.ReplyStrings)
	return reply.Datas, nil
}

func (client *Wallet) IsCaughtUp() bool {
	if client.client == nil {
		panic("wallet client not bind message queue.")
	}
	msg := client.client.NewMessage("blockchain", types.EventIsSync, nil)
	client.client.Send(msg, true)
	resp, err := client.client.Wait(msg)
	if err != nil {
		return false
	}
	return resp.GetData().(*types.IsCaughtUp).GetIscaughtup()
}

func (wallet *Wallet) tokenPreCreate(priv crypto.PrivKey, reqTokenPrcCreate *types.ReqTokenPreCreate) (*types.ReplyHash, error) {
	v := &types.TokenPreCreate{
		Name:         reqTokenPrcCreate.GetName(),
		Symbol:       reqTokenPrcCreate.GetSymbol(),
		Introduction: reqTokenPrcCreate.GetIntroduction(),
		Total:        reqTokenPrcCreate.GetTotal(),
		Price:        reqTokenPrcCreate.GetPrice(),
		Owner:        reqTokenPrcCreate.GetOwnerAddr(),
	}
	precreate := &types.TokenAction{
		Ty:    types.TokenActionPreCreate,
		Value: &types.TokenAction_Tokenprecreate{v},
	}
	tx := &types.Transaction{
		Execer:  []byte("token"),
		Payload: types.Encode(precreate),
		Fee:     wallet.FeeAmount,
		Nonce:   wallet.random.Int63(),
		To:      account.ExecAddress("token").String(),
	}
	tx.Sign(int32(SignType), priv)

	msg := wallet.client.NewMessage("mempool", types.EventTx, tx)
	wallet.client.Send(msg, true)
	resp, err := wallet.client.Wait(msg)
	if err != nil {
		walletlog.Error("procTokenPreCreate", "Send err", err)
		return nil, err
	}
	reply := resp.GetData().(*types.Reply)
	if !reply.GetIsOk() {
		return nil, errors.New(string(reply.GetMsg()))
	}
	var hash types.ReplyHash
	hash.Hash = tx.Hash()
	return &hash, nil
}

func (wallet *Wallet) tokenFinishCreate(priv crypto.PrivKey, req *types.ReqTokenFinishCreate) (*types.ReplyHash, error) {
	v := &types.TokenFinishCreate{Symbol: req.GetSymbol(), Owner: req.GetOwnerAddr()}
	finish := &types.TokenAction{
		Ty:    types.TokenActionFinishCreate,
		Value: &types.TokenAction_Tokenfinishcreate{v},
	}
	tx := &types.Transaction{
		Execer:  []byte("token"),
		Payload: types.Encode(finish),
		Fee:     wallet.FeeAmount,
		Nonce:   wallet.random.Int63(),
		To:      account.ExecAddress("token").String(),
	}
	tx.Sign(int32(SignType), priv)

	msg := wallet.client.NewMessage("mempool", types.EventTx, tx)
	wallet.client.Send(msg, true)
	resp, err := wallet.client.Wait(msg)
	if err != nil {
		walletlog.Error("procTokenFinishCreate", "Send err", err)
		return nil, err
	}
	reply := resp.GetData().(*types.Reply)
	if !reply.GetIsOk() {
		return nil, errors.New(string(reply.GetMsg()))
	}
	var hash types.ReplyHash
	hash.Hash = tx.Hash()
	return &hash, nil
}

func (wallet *Wallet) tokenRevokeCreate(priv crypto.PrivKey, req *types.ReqTokenRevokeCreate) (*types.ReplyHash, error) {
	v := &types.TokenRevokeCreate{Symbol: req.GetSymbol(), Owner: req.GetOwnerAddr()}
	revoke := &types.TokenAction{
		Ty:    types.TokenActionRevokeCreate,
		Value: &types.TokenAction_Tokenrevokecreate{v},
	}
	tx := &types.Transaction{
		Execer:  []byte("token"),
		Payload: types.Encode(revoke),
		Fee:     wallet.FeeAmount,
		Nonce:   wallet.random.Int63(),
		To:      account.ExecAddress("token").String(),
	}
	tx.Sign(int32(SignType), priv)

	msg := wallet.client.NewMessage("mempool", types.EventTx, tx)
	wallet.client.Send(msg, true)
	resp, err := wallet.client.Wait(msg)
	if err != nil {
		walletlog.Error("procTokenRevokeCreate", "Send err", err)
		return nil, err
	}
	reply := resp.GetData().(*types.Reply)
	if !reply.GetIsOk() {
		return nil, errors.New(string(reply.GetMsg()))
	}

	var hash types.ReplyHash
	hash.Hash = tx.Hash()
	return &hash, nil
}

func (wallet *Wallet) sellToken(priv crypto.PrivKey, reqSellToken *types.ReqSellToken) (*types.ReplyHash, error) {
	sell := &types.Trade{
		Ty:    types.TradeSell,
		Value: &types.Trade_Tokensell{reqSellToken.Sell},
	}
	tx := &types.Transaction{
		Execer:  []byte("trade"),
		Payload: types.Encode(sell),
		Fee:     wallet.FeeAmount,
		Nonce:   wallet.random.Int63(),
		To:      account.ExecAddress("trade").String(),
	}
	tx.Sign(int32(SignType), priv)

	msg := wallet.client.NewMessage("mempool", types.EventTx, tx)
	wallet.client.Send(msg, true)
	resp, err := wallet.client.Wait(msg)
	if err != nil {
		walletlog.Error("sellToken", "Send err", err)
		return nil, err
	}

	reply := resp.GetData().(*types.Reply)
	if !reply.GetIsOk() {
		return nil, errors.New(string(reply.GetMsg()))
	}
	var hash types.ReplyHash
	hash.Hash = tx.Hash()
	return &hash, nil
}

func (wallet *Wallet) buyToken(priv crypto.PrivKey, reqBuyToken *types.ReqBuyToken) (*types.ReplyHash, error) {
	buy := &types.Trade{
		Ty:    types.TradeBuy,
		Value: &types.Trade_Tokenbuy{reqBuyToken.Buy},
	}
	tx := &types.Transaction{
		Execer:  []byte("trade"),
		Payload: types.Encode(buy),
		Fee:     wallet.FeeAmount,
		Nonce:   wallet.random.Int63(),
		To:      account.ExecAddress("trade").String(),
	}
	tx.Sign(int32(SignType), priv)

	msg := wallet.client.NewMessage("mempool", types.EventTx, tx)
	wallet.client.Send(msg, true)
	resp, err := wallet.client.Wait(msg)
	if err != nil {
		walletlog.Error("buyToken", "Send err", err)
		return nil, err
	}

	reply := resp.GetData().(*types.Reply)
	if !reply.GetIsOk() {
		return nil, errors.New(string(reply.GetMsg()))
	}
	var hash types.ReplyHash
	hash.Hash = tx.Hash()
	return &hash, nil
}

func (wallet *Wallet) revokeSell(priv crypto.PrivKey, reqRevoke *types.ReqRevokeSell) (*types.ReplyHash, error) {
	revoke := &types.Trade{
		Ty:    types.TradeRevokeSell,
		Value: &types.Trade_Tokenrevokesell{reqRevoke.Revoke},
	}
	tx := &types.Transaction{
		Execer:  []byte("trade"),
		Payload: types.Encode(revoke),
		Fee:     wallet.FeeAmount,
		Nonce:   wallet.random.Int63(),
		To:      account.ExecAddress("trade").String(),
	}
	tx.Sign(int32(SignType), priv)

	msg := wallet.client.NewMessage("mempool", types.EventTx, tx)
	wallet.client.Send(msg, true)
	resp, err := wallet.client.Wait(msg)
	if err != nil {
		walletlog.Error("revoke sell token", "Send err", err)
		return nil, err
	}

	reply := resp.GetData().(*types.Reply)
	if !reply.GetIsOk() {
		return nil, errors.New(string(reply.GetMsg()))
	}
	var hash types.ReplyHash
	hash.Hash = tx.Hash()
	return &hash, nil
}

func (wallet *Wallet) modifyConfig(priv crypto.PrivKey, req *types.ReqModifyConfig) (*types.ReplyHash, error) {
	v := &types.ModifyConfig{Key: req.GetKey(), Op: req.GetOp(), Value: req.GetValue(), Addr: req.GetModifier()}
	modify := &types.ManageAction{
		Ty:    types.ManageActionModifyConfig,
		Value: &types.ManageAction_Modify{v},
	}
	tx := &types.Transaction{Execer: []byte("manage"), Payload: types.Encode(modify), Fee: wallet.FeeAmount, Nonce: wallet.random.Int63()}
	tx.Sign(int32(SignType), priv)

	msg := wallet.client.NewMessage("mempool", types.EventTx, tx)
	wallet.client.Send(msg, true)
	resp, err := wallet.client.Wait(msg)
	if err != nil {
		walletlog.Error("modifyConfig", "Send err", err)
		return nil, err
	}
	reply := resp.GetData().(*types.Reply)
	if !reply.GetIsOk() {
		return nil, errors.New(string(reply.GetMsg()))
	}

	var hash types.ReplyHash
	hash.Hash = tx.Hash()
	walletlog.Debug("modifyConfig", "sendTx", hash.Hash)
	return &hash, nil
}

func (wallet *Wallet) transPub2Pri(priv crypto.PrivKey, reqPub2Pri *types.ReqPub2Pri) (*types.ReplyHash, error) {
	privacyPtr := &privacy.Privacy{}

	viewPubSlice, err := common.FromHex(reqPub2Pri.ViewPublic)
	if err != nil {
		return nil, err
	}
	spendPubSlice, err := common.FromHex(reqPub2Pri.SpendPublic)
	if err != nil {
		return nil, err
	}

	if 32 != len(viewPubSlice) || 32 != len(spendPubSlice) {
		return nil, types.ErrPubKeyLen
	}

	viewPublic := (*[32]byte)(unsafe.Pointer(&viewPubSlice[0]))
	spendPublic := (*[32]byte)(unsafe.Pointer(&spendPubSlice[0]))

	pubkeyOnetime, txPublicKey, err := privacyPtr.GenerateOneTimeAddr(viewPublic, spendPublic)
	if err != nil {
		return nil, err
	}
	value := &types.Public2Privacy{
		Op4Token:    reqPub2Pri.Op4Token,
		Token:       reqPub2Pri.Token,
		Amount:      reqPub2Pri.Amount,
		Note:        reqPub2Pri.Note,
		RpubKeytx:   txPublicKey[:],
		OnetimePubKey: pubkeyOnetime[:],
	}
	action := &types.PrivacyAction{
		Ty:    types.ActionPublic2Privacy,
		Value: &types.PrivacyAction_Public2Privacy{value},
	}
	addrOneTime := account.PubKeyToAddress(pubkeyOnetime[:]).String()
	tx := &types.Transaction{
		Execer:  []byte("privacy"),
		Payload: types.Encode(action),
		Fee:     wallet.FeeAmount,
		Nonce:   wallet.random.Int63(),
		To:      addrOneTime,
	}
	tx.Sign(int32(SignType), priv)

	msg := wallet.client.NewMessage("mempool", types.EventTx, tx)
	wallet.client.Send(msg, true)
	resp, err := wallet.client.Wait(msg)
	if err != nil {
		walletlog.Error("revoke sell token", "Send err", err)
		return nil, err
	}

	reply := resp.GetData().(*types.Reply)
	if !reply.GetIsOk() {
		return nil, errors.New(string(reply.GetMsg()))
	}
	var hash types.ReplyHash
	hash.Hash = tx.Hash()
	return &hash, nil
}

func (wallet *Wallet) transPri2Pri(privacykeyParirs *privacy.Privacy, reqPri2Pri *types.ReqPri2Pri) (*types.ReplyHash, error) {
	viewPublicSlice, spendPublicSlice, err := convertPubPairstr2bytes(&reqPri2Pri.ViewPublic, &reqPri2Pri.SpendPublic)
	if err != nil {
		return nil, err
	}
	viewPublic := (*[32]byte)(unsafe.Pointer(&viewPublicSlice[0]))
	spendPublic := (*[32]byte)(unsafe.Pointer(&spendPublicSlice[0]))

	walletlog.Info("transPri2Pri", "viewPublic", viewPublic, "spendPublic", spendPublic,)

	pubkeyOnetime, txPublicKey, err := privacykeyParirs.GenerateOneTimeAddr(viewPublic, spendPublic)
	if err != nil {
		walletlog.Error("transPri2Pri", "Failed to GenerateOneTimeAddr")
		return nil, err
	}
	value := &types.Privacy2Privacy{
		Op4Token:    reqPri2Pri.Op4Token,
		Token:       reqPri2Pri.Token,
		Amount:      reqPri2Pri.Amount,
		Note:        reqPri2Pri.Note,
		RpubKeytx:   txPublicKey[:],
		OnetimePubKey: pubkeyOnetime[:],
	}
	action := &types.PrivacyAction{
		Ty:    types.ActionPrivacy2Privacy,
		Value: &types.PrivacyAction_Privacy2Privacy{value},
	}
	addrOneTime := account.PubKeyToAddress(pubkeyOnetime[:]).String()
	tx := &types.Transaction{
		Execer:  []byte("privacy"),
		Payload: types.Encode(action),
		Fee:     wallet.FeeAmount,
		Nonce:   wallet.random.Int63(),
		To:      addrOneTime,
	}

    txhash, err := common.FromHex(reqPri2Pri.Txhash)
    if err != nil {
    	return nil, err
	}
	var reqHashes types.ReqHashes
	reqHashes.Hashes = append(reqHashes.Hashes, txhash)

	R, err := wallet.GetRofPrivateTx(&reqHashes)
	if err != nil {
		walletlog.Error("transPri2Pri", "Failed to GetRofPrivateTx")
		return nil, err
	}
	walletlog.Info("transPri2Pri", "R of GetRofPrivateTx", R)
	//x = Hs(aR) + b
	priv, err := privacykeyParirs.RecoverOnetimePriKey(R, privacykeyParirs.ViewPrivKey, privacykeyParirs.SpendPrivKey)
	if err != nil {
		walletlog.Error("transPri2Pri", "Failed to RecoverOnetimePriKey", err)
		return nil, err
	}
	tx.Sign(int32(types.SignTypeOnetimeED25519), priv)

	msg := wallet.client.NewMessage("mempool", types.EventTx, tx)
	wallet.client.Send(msg, true)
	resp, err := wallet.client.Wait(msg)
	if err != nil {
		walletlog.Error("transPri2Pri", "Send err", err)
		return nil, err
	}

	reply := resp.GetData().(*types.Reply)
	if !reply.GetIsOk() {
		return nil, errors.New(string(reply.GetMsg()))
	}
	var hash types.ReplyHash
	hash.Hash = tx.Hash()
	return &hash, nil
}

func (wallet *Wallet) transPri2Pub(privacykeyParirs *privacy.Privacy, reqPri2Pub *types.ReqPri2Pub) (*types.ReplyHash, error) {
	value := &types.Privacy2Public{
		Op4Token:    reqPri2Pub.Op4Token,
		Token:       reqPri2Pub.Token,
		Amount:      reqPri2Pub.Amount,
		Note:        reqPri2Pub.Note,
	}
	action := &types.PrivacyAction{
		Ty:    types.ActionPrivacy2Public,
		Value: &types.PrivacyAction_Privacy2Public{value},
	}

	tx := &types.Transaction{
		Execer:  []byte("privacy"),
		Payload: types.Encode(action),
		Fee:     wallet.FeeAmount,
		Nonce:   wallet.random.Int63(),
		To:      reqPri2Pub.Receiver,
	}
    //获取隐私交易的tx public key
	txhash := []byte(reqPri2Pub.Txhash)
	txhahes := make([][]byte, 1)
	txhahes[0] = txhash
	var reqHashes types.ReqHashes
	reqHashes.Hashes = txhahes
	R, err := wallet.GetRofPrivateTx(&reqHashes)
	if err != nil {
		return nil, err
	}
	//x = Hs(aR) + b
	priv, err := privacykeyParirs.RecoverOnetimePriKey(R, privacykeyParirs.ViewPrivKey, privacykeyParirs.SpendPrivKey)
	if err != nil {
		return nil, err
	}
	tx.Sign(int32(types.SignTypeOnetimeED25519), priv)

	msg := wallet.client.NewMessage("mempool", types.EventTx, tx)
	wallet.client.Send(msg, true)
	resp, err := wallet.client.Wait(msg)
	if err != nil {
		walletlog.Error("revoke sell token", "Send err", err)
		return nil, err
	}

	reply := resp.GetData().(*types.Reply)
	if !reply.GetIsOk() {
		return nil, errors.New(string(reply.GetMsg()))
	}
	var hash types.ReplyHash
	hash.Hash = tx.Hash()
	return &hash, nil
}

func (wallet *Wallet) GetRofPrivateTx(ReqHashes *types.ReqHashes)(R_txpubkey []byte, err error) {
	//通过txhashs获取对应的txdetail
	msg := wallet.client.NewMessage("blockchain", types.EventGetTransactionByHash, ReqHashes)
	wallet.client.Send(msg, true)
	resp, err := wallet.client.Wait(msg)
	if err != nil {
		walletlog.Error("GetRofPrivateTx EventGetTransactionByHash", "err", err)
		return nil, err
	}
	TxDetails := resp.GetData().(*types.TransactionDetails)
	if TxDetails == nil {
		walletlog.Error("GetRofPrivateTx TransactionDetails is nil")
		return nil, errors.New("ErrTxDetail")
	}

	if "privacy" != string(TxDetails.Txs[0].Tx.Execer) {
		walletlog.Error("GetRofPrivateTx get tx but not privacy")
		return nil, errors.New("ErrPrivacyExecer")
	}

	var privateAction types.PrivacyAction
	if err := types.Decode(TxDetails.Txs[0].Tx.Payload, &privateAction); err != nil {
		walletlog.Error("GetRofPrivateTx failed to decode payload")
		return nil, errors.New("ErrPrivacyPayload")
	}

	if types.ActionPublic2Privacy == privateAction.Ty {
		return privateAction.GetPublic2Privacy().GetRpubKeytx(), nil
	} else if types.ActionPrivacy2Privacy == privateAction.Ty {
		return privateAction.GetPrivacy2Privacy().GetRpubKeytx(), nil
	} else {
		walletlog.Info("GetPrivateTxByHashes failed to get value required", "privacy type is", privateAction.Ty)
		return nil, errors.New("ErrPrivacyType")
	}
}

func convertPubPairstr2bytes(viewPub, spendPub *string) (viewPubSlice, spendPubSlice []byte, err error) {
	viewPubSlice, err = common.FromHex(*viewPub)
	if err != nil {
		return nil, nil, err
	}
	spendPubSlice, err = common.FromHex(*spendPub)
	if err != nil {
		return nil, nil, err
	}

	if 32 != len(viewPubSlice) || 32 != len(spendPubSlice) {
		return nil, nil, types.ErrPubKeyLen
	}

	return
}


