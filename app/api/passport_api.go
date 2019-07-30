package api

import (
	"errors"
	"fmt"
	"github.com/ixre/gof"
	"github.com/ixre/gof/api"
	"github.com/ixre/gof/storage"
	"github.com/ixre/gof/util"
	"go2o/core/domain/interface/member"
	"go2o/core/domain/interface/mss/notify"
	"go2o/core/domain/interface/registry"
	"go2o/core/infrastructure/domain"
	"go2o/core/service/auto_gen/rpc/member_service"
	"go2o/core/service/auto_gen/rpc/message_service"
	"go2o/core/service/rsi"
	"go2o/core/service/thrift"
	"log"
	"strconv"
	"strings"
	"time"
)

var _ api.Handler = new(PassportApi)

var (
	operationArr = []string{"找回密码", "重置密码", "绑定手机"}
	//2分钟后才可重发验证码
	timeOutUnix int64 = 120 //等于:time.Unix(120,0).Unix()
)

type PassportApi struct {
	apiUtil
	st storage.Interface
}

func NewPassportApi() api.Handler {
	st := gof.CurrentApp.Storage()
	return &PassportApi{
		st: st,
	}
}

func (m PassportApi) Process(fn string, ctx api.Context) *api.Response {
	return api.HandleMultiFunc(fn, ctx, map[string]api.HandlerFunc{
		"send_code":    m.SendCode,
		"compare_code": m.CompareCode,
		"reset_pwd":    m.ResetPwdPost,
		"modify_pwd":   m.ModifyPwdPost,
		"trade_pwd":    m.ModifyPwdPost,
	})
}

// 根据输入的凭据获取会员编号
func (m PassportApi) checkMemberBasis(ctx api.Context) (string, int, error) {
	acc := strings.TrimSpace(ctx.Form().GetString("basis"))    //账号、手机或邮箱
	msgType, err := strconv.Atoi(ctx.Form().GetString("type")) //发送方式、短信或邮箱
	if err != nil {
		return acc, msgType, err
	}
	if len(acc) == 0 {
		return acc, msgType, errors.New("信息不完整")
	}
	return acc, msgType, nil
}

// 根据发送的校验码类型获取用户凭据类型
func (h PassportApi) getMemberCredential(msgType int) member_service.ECredentials {
	if msgType == notify.TypeEmailMessage {
		return member_service.ECredentials_Email
	} else if msgType == notify.TypePhoneMessage {
		return member_service.ECredentials_Phone
	}
	return member_service.ECredentials_User
}

// 标记验证码发送时间
func (h PassportApi) signCodeSendInfo(token string) {
	prefix := "sys:go2o:pwd:token"
	// 最后的发送时间
	unix := time.Now().Unix()
	h.st.SetExpire(fmt.Sprintf("%s:%s:last_unix", prefix, token), unix, 600)
	// 验证码校验成功
	h.st.SetExpire(fmt.Sprintf("%s:%s:check_ok", prefix, token), 0, 600)
	// 清除记录的会员编号
	h.st.Del(fmt.Sprintf("%s:%s:member_id", prefix, token))
}

// 获取校验结果
func (p PassportApi) GetCodeVerifyResult(token string) (memberId int64, result bool) {
	prefix := "sys:go2o:pwd:token"
	checkKey := fmt.Sprintf("%s:%s:check_ok", prefix, token)
	v, err := p.st.GetInt64(checkKey)
	b := err == nil && v == 1 //验证码校验成功
	if b {
		mmKey := fmt.Sprintf("%s:%s:member_id", prefix, token)
		if memberId, err := p.st.GetInt64(mmKey); err == nil {
			return memberId, false
		}
		return memberId, true
	}
	return memberId, false
}

// 清理验证码校验结果
func (p PassportApi) resetCodeVerifyResult(token string) {
	prefix := "sys:go2o:pwd:token"
	checkKey := fmt.Sprintf("%s:%s:check_ok", prefix, token)
	p.st.Del(checkKey)
}

// 设置校验成功
func (p PassportApi) setCodeVerifySuccess(token string, memberId int64) {
	prefix := "sys:go2o:pwd:token"
	checkKey := fmt.Sprintf("%s:%s:check_ok", prefix, token)
	mmKey := fmt.Sprintf("%s:%s:member_id", prefix, token)
	p.st.SetExpire(checkKey, 1, 600)     // 验证码校验成功
	p.st.SetExpire(mmKey, memberId, 600) // 记录会员编号
}

/**
 * @api {post} /passport/send_code 发送验证码
 * @apiName send_code
 * @apiGroup passport
 * @apiParam {String} token 注册令牌
 * @apiParam {Int} op 验证码场景:0:找回密码, 1:重置密码 2:绑定手机
 * @apiParam {String} basis 账号
 * @apiParam {Int} type 验证码类型,1: 站内信 2: 短信 3:邮箱,
 * @apiSuccessExample Success-Response
 * {}
 * @apiSuccessExample Error-Response
 * {"code":1,"message":"api not defined"}
 */
func (h PassportApi) SendCode(ctx api.Context) interface{} {
	token := strings.TrimSpace(ctx.Form().GetString("token"))
	if len(token) == 0 {
		return api.ResponseWithCode(6, "非法注册请求")
	}
	operation, _ := strconv.Atoi(ctx.Form().GetString("op")) //操作
	account, msgType, err := h.checkMemberBasis(ctx)
	if err != nil {
		return api.ResponseWithCode(2, err.Error())
	}
	trans, cli, err := thrift.MemberServeClient()
	if err == nil {
		defer trans.Close()
		var cred = h.getMemberCredential(msgType)
		memberId, _ := cli.SwapMemberId(thrift.Context, cred, account)
		if memberId <= 0 {
			return api.ResponseWithCode(1, member.ErrNoSuchMember.Error())
		}
		err = h.checkCodeDuration(token, account)
		if err == nil {
			r, _ := cli.SendCode(thrift.Context, memberId,
				operationArr[operation],
				message_service.EMessageChannel(msgType))
			code := r.Data["code"]
			if r.ErrCode == 0 {
				h.signCodeSendInfo(token) // 标记为已发送
			} else {
				log.Println("[ Go2o][ Error]: 发送会员验证码失败:", r.ErrMsg)
				err = errors.New("发送验证码失败")
			}
			keys := []string{
				registry.EnableDebugMode,
			}
			trans, cli, _ := thrift.FoundationServeClient()
			mp, _ := cli.GetRegistries(thrift.Context, keys)
			trans.Close()
			debugMode := mp[keys[0]] == "true"
			if debugMode && len(code) != 0 {
				return api.ResponseWithCode(3, "【测试】短信验证码为:"+code)
			}
		}
	}
	if err != nil {
		return api.ResponseWithCode(1, err.Error())
	}
	return api.NewResponse(map[string]string{})
}

// 提交重置密码

/**
 * @api {post} /passport/reset_pwd 重置密码
 * @apiName send_code
 * @apiGroup passport
 * @apiParam {String} token 注册令牌
 * @apiParam {String} pwd 密码
 * @apiParam {String} repwd 确认密码
 * @apiSuccessExample Success-Response
 * {}
 * @apiSuccessExample Error-Response
 * {"code":1,"message":"api not defined"}
 */
func (p PassportApi) ResetPwdPost(ctx api.Context) interface{} {
	token := strings.TrimSpace(ctx.Form().GetString("token"))
	if len(token) == 0 {
		return api.ResponseWithCode(6, "非法注册请求")
	}
	memberId, b := p.GetCodeVerifyResult(token)
	if !b {
		return api.ResponseWithCode(2, "验证码不正确")
	}
	var err error
	newPwd := domain.Md5(strings.TrimSpace(ctx.Form().GetString("pwd")))
	rePwd := domain.Md5(strings.TrimSpace(ctx.Form().GetString("repwd")))
	if len(newPwd) == 0 {
		err = errors.New("密码不能为空")
	} else if newPwd != rePwd {
		err = errors.New("两次密码输入不一致")
	} else {
		err = rsi.MemberService.ModifyPassword(memberId, newPwd, "")
		if err == nil {
			p.resetCodeVerifyResult(token)
		}
	}
	if err != nil {
		return api.ResponseWithCode(1, err.Error())
	}
	return api.NewResponse(map[string]string{})
}

// 提交修改密码
func (p PassportApi) ModifyPwdPost(ctx api.Context) interface{} {
	token := strings.TrimSpace(ctx.Form().GetString("token"))
	if len(token) == 0 {
		return api.ResponseWithCode(6, "非法注册请求")
	}
	memberId, b := p.GetCodeVerifyResult(token)
	if !b {
		return api.ResponseWithCode(2, "验证码不正确")
	}
	var err error
	oldPwd := domain.Md5(strings.TrimSpace(ctx.Form().GetString("OldPwd")))
	newPwd := domain.Md5(strings.TrimSpace(ctx.Form().GetString("pwd")))
	rePwd := domain.Md5(strings.TrimSpace(ctx.Form().GetString("repwd")))
	if oldPwd == "" {
		err = errors.New("旧密码不能为空")
	} else if len(newPwd) == 0 {
		err = errors.New("新密码不能为空")
	} else if newPwd != rePwd {
		err = errors.New("两次密码输入不一致")
	} else {
		err = rsi.MemberService.ModifyPassword(memberId, newPwd, oldPwd)
		if err == nil {
			p.resetCodeVerifyResult(token)
		}
	}
	if err != nil {
		return api.ResponseWithCode(1, err.Error())
	}
	return api.NewResponse(map[string]string{})
}

// 提交修改交易密码
func (p PassportApi) TradePwdPost(ctx api.Context) *api.Response {
	token := strings.TrimSpace(ctx.Form().GetString("token"))
	if len(token) == 0 {
		return api.ResponseWithCode(6, "非法注册请求")
	}
	memberId, b := p.GetCodeVerifyResult(token)
	if !b {
		return api.ResponseWithCode(2, "验证码不正确")
	}
	var err error
	oldPwd := domain.Md5(strings.TrimSpace(ctx.Form().GetString("OldPwd")))
	newPwd := domain.Md5(strings.TrimSpace(ctx.Form().GetString("pwd")))
	rePwd := domain.Md5(strings.TrimSpace(ctx.Form().GetString("repwd")))
	if oldPwd == "" {
		err = errors.New("旧密码不能为空")
	} else if len(newPwd) == 0 {
		err = errors.New("新密码不能为空")
	} else if newPwd != rePwd {
		err = errors.New("两次密码输入不一致")
	} else {
		err = rsi.MemberService.ModifyTradePassword(memberId, newPwd, oldPwd)
		if err == nil {
			p.resetCodeVerifyResult(token)
		}
	}
	if err != nil {
		return api.ResponseWithCode(1, err.Error())
	}
	return api.NewResponse(map[string]string{})

	//
	//if oldPwd == "" {
	//	m := GetMember(c)
	//	if m.TradePwd != "" {
	//		err := errors.New("请输入原交易密码")
	//		return c.JSON(http.StatusOK, result.Error(err))
	//	}
	//}

	/*
		var err error
		if newPwd != rePwd {
			err = errors.New("两次密码输入不一致")
		} else {
			err = rsi.MemberService.ModifyTradePassword(int64(memberId), oldPwd, newPwd)
		}
		return c.JSON(http.StatusOK, result.Error(err))
	*/
}

// 对比验证码
func (h PassportApi) CompareCode(ctx api.Context) interface{} {
	token := strings.TrimSpace(ctx.Form().GetString("token"))
	if len(token) == 0 {
		return api.ResponseWithCode(6, "非法注册请求")
	}
	account, msgType, err := h.checkMemberBasis(ctx)
	if err != nil {
		return api.ResponseWithCode(2, err.Error())
	}
	code := ctx.Form().GetString("code")
	trans, cli, err := thrift.MemberServeClient()
	if err == nil {
		defer trans.Close()
		var cred = h.getMemberCredential(msgType)
		memberId, _ := cli.SwapMemberId(thrift.Context, cred, account)
		r, _ := cli.CompareCode(thrift.Context, memberId, code)
		if r.ErrCode == 0 {
			h.setCodeVerifySuccess(token, memberId)
		} else {
			err = errors.New(r.ErrMsg)
		}
	}
	if err != nil {
		return api.ResponseWithCode(1, err.Error())
	}
	return api.NewResponse(map[string]string{})
}

/**
 * @api {post} /register/get_token 获取注册Token
 * @apiName get_token
 * @apiGroup register
 * @apiSuccessExample Success-Response
 * {}
 * @apiSuccessExample Error-Response
 * {"code":1,"message":"api not defined"}
 */
func (m PassportApi) getToken(ctx api.Context) interface{} {
	rd := util.RandString(10)
	key := fmt.Sprintf("sys:go2o:reg:token:%s:last-time", rd)
	m.st.SetExpire(key, 0, 600)
	return rd
}

// 获取验证码的间隔时间
func (m PassportApi) getDurationSecond() int64 {
	trans, cli, err := thrift.FoundationServeClient()
	if err == nil {
		val, _ := cli.GetRegistry(thrift.Context, registry.SmsSendDuration)
		trans.Close()
		i, err := strconv.Atoi(val)
		if err != nil {
			log.Println("[ Go2o][ Registry]: parse value error:", err.Error())
		}
		return int64(i)
	}
	return 120
}

// 检查短信验证码是否频繁发送
func (m PassportApi) checkCodeDuration(token, phone string) error {
	key := fmt.Sprintf("sys:go2o:reg:token:%s:last-time", token)
	nowUnix := time.Now().Unix()
	unix, err := m.st.GetInt64(key)

	log.Println("---", nowUnix, unix, key)

	if err == nil {
		if nowUnix-unix < m.getDurationSecond() {
			return errors.New("请勿在短时间内获取短信验证码!")
		}
	}
	return nil
}

// 标记验证码发送时间
func (m PassportApi) signCheckCodeSendOk(token string) {
	key := fmt.Sprintf("sys:go2o:reg:token:%s:last-time", token)
	unix := time.Now().Unix()
	log.Println("----save code:", unix)
	m.st.SetExpire(key, unix, 600)
}

// 验证注册令牌是否正确
func (m PassportApi) checkRegToken(token string) bool {
	key := fmt.Sprintf("sys:go2o:reg:token:%s:last-time", token)
	_, err := m.st.GetInt64(key)
	return err == nil
}

// 将注册令牌标记为过期
func (m PassportApi) signCheckTokenExpires(token string) {
	key := fmt.Sprintf("sys:go2o:reg:token:%s:last-time", token)
	m.st.Del(key)
}

// 存储校验数据
func (m PassportApi) saveCheckCodeData(token string, phone string, code string) {
	key := fmt.Sprintf("sys:go2o:reg:token:%s:reg_check_code", token)
	key1 := fmt.Sprintf("sys:go2o:reg:token:%s:reg_check_phone", token)
	m.st.SetExpire(key, code, 600)
	m.st.SetExpire(key1, phone, 600)
}