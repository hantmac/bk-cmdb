/*
 * Tencent is pleased to support the open source community by making 蓝鲸 available.
 * Copyright (C) 2017-2018 THL A29 Limited, a Tencent company. All rights reserved.
 * Licensed under the MIT License (the "License"); you may not use this file except
 * in compliance with the License. You may obtain a copy of the License at
 * http://opensource.org/licenses/MIT
 * Unless required by applicable law or agreed to in writing, software distributed under
 * the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
 * either express or implied. See the License for the specific language governing permissions and
 * limitations under the License.
 */

package service

import (
	"encoding/json"
	"io/ioutil"
	"strconv"
	"strings"

	"configcenter/src/ac"
	"configcenter/src/ac/extensions"
	authmeta "configcenter/src/ac/meta"
	"configcenter/src/common"
	"configcenter/src/common/blog"
	"configcenter/src/common/errors"
	"configcenter/src/common/http/rest"
	"configcenter/src/common/mapstr"
	meta "configcenter/src/common/metadata"
	"configcenter/src/common/util"
	"configcenter/src/scene_server/host_server/logics"
	hutil "configcenter/src/scene_server/host_server/util"
)

type AppResult struct {
	Result  bool        `json:"result"`
	Code    int         `json:"code"`
	Message interface{} `json:"message"`
	Data    DataInfo    `json:"data"`
}

type DataInfo struct {
	Count int                      `json:"count"`
	Info  []map[string]interface{} `json:"info"`
}

// delete hosts from resource pool
func (s *Service) DeleteHostBatchFromResourcePool(ctx *rest.Contexts) {

	opt := new(meta.DeleteHostBatchOpt)
	if err := ctx.DecodeInto(&opt); nil != err {
		ctx.RespAutoError(err)
		return
	}

	hostIDArr := strings.Split(opt.HostID, ",")
	var iHostIDArr []int64
	delCondsArr := make([][]map[string]interface{}, 0)
	for _, i := range hostIDArr {
		iHostID, err := strconv.ParseInt(i, 10, 64)
		if err != nil {
			blog.Errorf("delete host batch, but got invalid host id, err: %v,input:%+v,rid:%s", err, opt, ctx.Kit.Rid)
			ctx.RespAutoError(ctx.Kit.CCError.CCErrorf(common.CCErrCommParamsInvalid, iHostID))
			return
		}
		iHostIDArr = append(iHostIDArr, iHostID)
	}
	iHostIDArr = util.IntArrayUnique(iHostIDArr)

	// auth: check authorization
	if err := s.AuthManager.AuthorizeByHostsIDs(ctx.Kit.Ctx, ctx.Kit.Header, authmeta.Delete, iHostIDArr...); err != nil {
		blog.Errorf("check host authorization failed, hosts: %+v, err: %v, rid: %s", iHostIDArr, err, ctx.Kit.Rid)
		if err != ac.NoAuthorizeError {
			ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrHostDeleteFail))
			return
		}
		perm, err := s.AuthManager.GenHostBatchNoPermissionResp(ctx.Kit.Ctx, ctx.Kit.Header, authmeta.Delete, iHostIDArr)
		if err != nil {
			ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrHostDeleteFail))
			return
		}
		ctx.RespEntityWithError(perm, ac.NoAuthorizeError)
		return
	}

	for _, iHostID := range iHostIDArr {
		asstCond := map[string]interface{}{
			common.BKDBOR: []map[string]interface{}{
				{
					common.BKObjIDField:  common.BKInnerObjIDHost,
					common.BKHostIDField: iHostID,
				},
				{
					common.BKAsstObjIDField:  common.BKInnerObjIDHost,
					common.BKAsstInstIDField: iHostID,
				},
			},
		}
		rsp, err := s.CoreAPI.CoreService().Association().ReadInstAssociation(ctx.Kit.Ctx, ctx.Kit.Header, &meta.QueryCondition{Condition: asstCond})
		if nil != err {
			blog.ErrorJSON("DeleteHostBatch read host association do request failed , err: %s, rid: %s", err.Error(), ctx.Kit.Rid)
			ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommHTTPDoRequestFailed))
			return
		}
		if !rsp.Result {
			blog.ErrorJSON("DeleteHostBatch read host association failed , err message: %s, rid: %s", rsp.ErrMsg, ctx.Kit.Rid)
			ctx.RespAutoError(rsp.CCError())
			return
		}
		if rsp.Data.Count <= 0 {
			continue
		}
		objIDs := make([]string, 0)
		asstInstMap := make(map[string][]int64, 0)
		for _, asst := range rsp.Data.Info {
			if asst.ObjectID == common.BKInnerObjIDHost && iHostID == asst.InstID {
				objIDs = append(objIDs, asst.AsstObjectID)
				asstInstMap[asst.AsstObjectID] = append(asstInstMap[asst.AsstObjectID], asst.AsstInstID)
			} else if asst.AsstObjectID == common.BKInnerObjIDHost && iHostID == asst.AsstInstID {
				objIDs = append(objIDs, asst.ObjectID)
				asstInstMap[asst.ObjectID] = append(asstInstMap[asst.ObjectID], asst.InstID)
			} else {
				ctx.RespAutoError(ctx.Kit.CCError.New(common.CCErrCommDBSelectFailed, "host is not associated in selected association"))
				return
			}
		}
		delConds := make([]map[string]interface{}, 0)
		for objID, instIDs := range asstInstMap {
			if len(instIDs) < 0 {
				continue
			}
			instIDField := common.GetInstIDField(objID)
			instCond := map[string]interface{}{
				instIDField: map[string]interface{}{
					common.BKDBIN: instIDs,
				},
			}
			instRsp, err := s.CoreAPI.CoreService().Instance().ReadInstance(ctx.Kit.Ctx, ctx.Kit.Header, objID, &meta.QueryCondition{Condition: instCond})
			if err != nil {
				blog.ErrorJSON("DeleteHostBatch read associated instances do request failed , err: %s, rid: %s", err.Error(), ctx.Kit.Rid)
				ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommHTTPDoRequestFailed))
				return
			}
			if !instRsp.Result {
				blog.ErrorJSON("DeleteHostBatch read associated instances failed , err message: %s, rid: %s", instRsp.ErrMsg, ctx.Kit.Rid)
				ctx.RespAutoError(instRsp.CCError())
				return
			}
			if len(instRsp.Data.Info) > 0 {
				blog.ErrorJSON("DeleteHostBatch host %s has been associated, can't be deleted, rid: %s", iHostID, ctx.Kit.Rid)
				ctx.RespAutoError(ctx.Kit.CCError.CCErrorf(common.CCErrTopoInstHasBeenAssociation, iHostID))
				return
			}
			delConds = append(delConds, map[string]interface{}{
				common.BKObjIDField: objID,
				instIDField: map[string]interface{}{
					common.BKDBIN: instIDs,
				},
			}, map[string]interface{}{
				common.AssociatedObjectIDField: objID,
				instIDField: map[string]interface{}{
					common.BKDBIN: instIDs,
				},
			})
		}
		if len(delConds) > 0 {
			delCondsArr = append(delCondsArr, delConds)
		}
	}

	txnErr := s.Engine.CoreAPI.CoreService().Txn().AutoRunTxn(ctx.Kit.Ctx, s.EnableTxn, ctx.Kit.Header, func() error {
		for _, delConds := range delCondsArr {
			delRsp, err := s.CoreAPI.CoreService().Association().DeleteInstAssociation(ctx.Kit.Ctx, ctx.Kit.Header, &meta.DeleteOption{Condition: map[string]interface{}{common.BKDBOR: delConds}})
			if err != nil {
				blog.ErrorJSON("DeleteHostBatch delete host redundant association do request failed , err: %s, rid: %s", err.Error(), ctx.Kit.Rid)
				return ctx.Kit.CCError.CCError(common.CCErrCommHTTPDoRequestFailed)
			}
			if !delRsp.Result {
				blog.ErrorJSON("DeleteHostBatch delete host redundant association failed , err message: %s, rid: %s", delRsp.ErrMsg, ctx.Kit.Rid)
				return delRsp.CCError()
			}
		}
		lgc := logics.NewLogics(s.Engine, ctx.Kit.Header, s.CacheDB, s.AuthManager)
		appID, err := lgc.GetDefaultAppID(ctx.Kit.Ctx)
		if err != nil {
			blog.Errorf("delete host batch, but got invalid app id, err: %v,input:%s,rid:%s", err, opt, ctx.Kit.Rid)
			return ctx.Kit.CCError.Errorf(common.CCErrCommParamsNeedInt, common.BKAppIDField)
		}

		hostFields, err := lgc.GetHostAttributes(ctx.Kit.Ctx, ctx.Kit.SupplierAccount, meta.BizLabelNotExist)
		if err != nil {
			blog.Errorf("delete host batch failed, err: %v,input:%+v,rid:%s", err, opt, ctx.Kit.Rid)
			return err
		}

		logContentMap := make(map[int64]meta.AuditLog, 0)
		hosts := make([]extensions.HostSimplify, 0)
		for _, hostID := range iHostIDArr {
			logger := lgc.NewHostLog(ctx.Kit.Ctx, ctx.Kit.SupplierAccount)
			if err := logger.WithPrevious(ctx.Kit.Ctx, hostID, hostFields); err != nil {
				blog.Errorf("delete host batch, but get pre host data failed, err: %v,input:%+v,rid:%s", err, opt, ctx.Kit.Rid)
				return err
			}

			logContentMap[hostID], err = logger.AuditLog(ctx.Kit.Ctx, hostID, appID, meta.AuditDelete)
			if err != nil {
				blog.Errorf("delete host batch, but get host[%d] biz[%d] data failed, err: %v, rid:%s", hostID, appID, err, ctx.Kit.Rid)
				return err
			}

			detail, ok := logContentMap[hostID].OperationDetail.(*meta.InstanceOpDetail)
			if !ok {
				blog.Errorf("delete host batch, but got invalid operation detail, rid:%s", ctx.Kit.Rid)
				return errors.New(common.CCErrCommParamsValueInvalidError, "")
			}

			hosts = append(hosts, extensions.HostSimplify{
				BKAppIDField:       0,
				BKHostIDField:      hostID,
				BKHostInnerIPField: detail.ResourceName,
			})
		}

		input := &meta.DeleteHostRequest{
			ApplicationID: appID,
			HostIDArr:     iHostIDArr,
		}
		delResult, err := s.CoreAPI.CoreService().Host().DeleteHostFromSystem(ctx.Kit.Ctx, ctx.Kit.Header, input)
		if err != nil {
			blog.Error("DeleteHostBatch DeleteHost http do error. err:%s, input:%s, rid:%s", err.Error(), input, ctx.Kit.Rid)
			return ctx.Kit.CCError.CCError(common.CCErrCommHTTPDoRequestFailed)
		}
		if !delResult.Result {
			blog.Errorf("DeleteHostBatch DeleteHost http reply error. result: %#v, input:%#v, rid:%s", delResult, input, ctx.Kit.Rid)
			return ctx.Kit.CCError.CCError(common.CCErrHostDeleteFail)
		}

		// ensure delete host add log
		for _, ex := range delResult.Data {
			delete(logContentMap, ex.OriginIndex)
		}
		var logContents []meta.AuditLog
		for _, item := range logContentMap {
			logContents = append(logContents, item)
		}
		if len(logContents) > 0 {
			auditResult, err := s.CoreAPI.CoreService().Audit().SaveAuditLog(ctx.Kit.Ctx, ctx.Kit.Header, logContents...)
			if err != nil || !auditResult.Result {
				blog.ErrorJSON("delete host in batch, but add host audit log failed, err: %s, result: %s,rid:%s", err, auditResult, ctx.Kit.Rid)
				return ctx.Kit.CCError.CCError(common.CCErrAuditSaveLogFailed)
			}
		}
		return nil
	})

	if txnErr != nil {
		ctx.RespAutoError(txnErr)
		return
	}
	ctx.RespEntity(nil)
}

// get host instance's properties as follows:
// host object property id: "bk_host_name"
// host object property name: "host"
// host object property value: "centos7"

func (s *Service) GetHostInstanceProperties(ctx *rest.Contexts) {

	hostID := ctx.Request.PathParameter("bk_host_id")
	hostIDInt64, err := util.GetInt64ByInterface(hostID)
	if err != nil {
		blog.Errorf("convert hostID to int64, err: %v,host:%s,rid:%s", err, hostID, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCErrorf(common.CCErrCommParamsNeedInt, common.BKHostIDField))
		return
	}
	lgc := logics.NewLogics(s.Engine, ctx.Kit.Header, s.CacheDB, s.AuthManager)
	details, _, err := lgc.GetHostInstanceDetails(ctx.Kit.Ctx, hostIDInt64)
	if err != nil {
		blog.Errorf("get host details failed, err: %v,host:%s,rid:%s", err, hostID, ctx.Kit.Rid)
		ctx.RespAutoError(err)
		return
	}
	if len(details) == 0 {
		blog.Errorf("host not found, hostID: %v,rid:%s", hostID, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrHostNotFound))
		return
	}

	// check authorization
	// auth: check authorization
	if err := s.AuthManager.AuthorizeByHostsIDs(ctx.Kit.Ctx, ctx.Kit.Header, authmeta.Find, hostIDInt64); err != nil {
		blog.Errorf("check host authorization failed, hosts: %+v, err: %v, rid: %s", hostIDInt64, err, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommAuthorizeFailed))
		return
	}
	attribute, err := lgc.GetHostAttributes(ctx.Kit.Ctx, ctx.Kit.SupplierAccount, nil)
	if err != nil {
		blog.Errorf("get host attribute fields failed, err: %v,rid:%s", err, ctx.Kit.Rid)
		ctx.RespAutoError(err)
		return
	}

	result := make([]meta.HostInstanceProperties, 0)
	for _, attr := range attribute {
		if attr.PropertyID == common.BKChildStr {
			continue
		}
		result = append(result, meta.HostInstanceProperties{
			PropertyID:    attr.PropertyID,
			PropertyName:  attr.PropertyName,
			PropertyValue: details[attr.PropertyID],
		})
	}

	ctx.RespEntity(result)

}

// HostSnapInfo return host state
func (s *Service) HostSnapInfo(ctx *rest.Contexts) {

	hostID := ctx.Request.PathParameter(common.BKHostIDField)
	hostIDInt64, err := strconv.ParseInt(hostID, 10, 64)
	if err != nil {
		blog.Errorf("HostSnapInfo hostID convert to int64 failed, err:%v, input:%+v, rid:%s", err, hostID, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommParamsNeedInt))
		return
	}

	// check authorization
	// auth: check authorization
	if err := s.AuthManager.AuthorizeByHostsIDs(ctx.Kit.Ctx, ctx.Kit.Header, authmeta.Find, hostIDInt64); err != nil {
		blog.Errorf("check host authorization failed, hosts: %+v, err: %v, rid: %s", hostIDInt64, err, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommAuthorizeFailed))
		return
	}

	// get snapshot
	result, err := s.CoreAPI.CoreService().Host().GetHostSnap(ctx.Kit.Ctx, ctx.Kit.Header, hostID)

	if err != nil {
		blog.Errorf("HostSnapInfo, http do error, err: %v ,input:%#v, rid:%s", err, hostID, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommHTTPReadBodyFailed))
		return
	}
	if !result.Result {
		blog.Errorf("HostSnapInfo, http response error, err code:%d,err msg:%s, input:%#v, rid:%s", result.Code, result.ErrMsg, hostID, ctx.Kit.Rid)
		ctx.RespAutoError(result.CCError())
		return
	}

	snap, err := logics.ParseHostSnap(result.Data.Data)
	if err != nil {
		blog.Errorf("get host snap info, but parse snap info failed, err: %v, hostID:%v,rid:%s", err, hostID, ctx.Kit.Rid)
		ctx.RespAutoError(err)
		return
	}

	ctx.RespEntity(snap)

}

// HostSnapInfoBatch get the host snapshot in batch
func (s *Service) HostSnapInfoBatch(ctx *rest.Contexts) {

	option := meta.SearchInstBatchOption{}
	if err := json.NewDecoder(ctx.Request.Request.Body).Decode(&option); err != nil {
		blog.Errorf("HostSnapInfoBatch failed, decode body err: %v, rid:%s", err, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommJSONUnmarshalFailed))
		return
	}

	rawErr := option.Validate()
	if rawErr.ErrCode != 0 {
		ctx.RespAutoError(rawErr.ToCCError(ctx.Kit.CCError))
		return
	}

	hostIDs := util.IntArrayUnique(option.IDs)

	// check authorization
	// auth: check authorization
	if err := s.AuthManager.AuthorizeByHostsIDs(ctx.Kit.Ctx, ctx.Kit.Header, authmeta.Find, hostIDs...); err != nil {
		blog.Errorf("check host authorization failed, hostIDs: %#v, err: %v, rid: %s", hostIDs, err, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommAuthorizeFailed))
		return
	}

	input := meta.HostSnapBatchInput{HostIDs: hostIDs}
	// get snapshot
	result, err := s.CoreAPI.CoreService().Host().GetHostSnapBatch(ctx.Kit.Ctx, ctx.Kit.Header, input)
	if err != nil {
		blog.Errorf("HostSnapInfoBatch failed, http do error, err: %v ,input:%#v, rid:%s", err, input, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommHTTPReadBodyFailed))
		return
	}
	if !result.Result {
		blog.Errorf("HostSnapInfoBatch failed, http response error, err code:%d, err msg:%s, input:%#v, rid:%s", result.Code, result.ErrMsg, input, ctx.Kit.Rid)
		ctx.RespAutoError(result.CCError())
		return
	}

	ret := make([]map[string]interface{}, 0)
	for hostID, snapData := range result.Data {
		if snapData == "" {
			blog.Infof("snapData is empty, hostID:%v, rid:%s", hostID, ctx.Kit.Rid)
			ret = append(ret, map[string]interface{}{"bk_host_id": hostID})
			continue
		}
		snap, err := logics.ParseHostSnap(snapData)
		if err != nil {
			blog.Errorf("HostSnapInfoBatch failed, ParseHostSnap err: %v, hostID:%v, rid:%s", err, hostID, ctx.Kit.Rid)
			ctx.RespAutoError(err)
			return
		}
		snapFields := make(map[string]interface{})
		for _, field := range option.Fields {
			if _, ok := snap[field]; ok {
				snapFields[field] = snap[field]
			}
		}
		snapFields["bk_host_id"] = hostID
		ret = append(ret, snapFields)
	}

	ctx.RespEntity(ret)

}

// add host to host resource pool
func (s *Service) AddHost(ctx *rest.Contexts) {
	hostList := new(meta.HostList)
	if err := ctx.DecodeInto(&hostList); nil != err {
		ctx.RespAutoError(err)
		return
	}

	if hostList.HostInfo == nil {
		blog.Errorf("add host, but host info is nil.input:%+v,rid:%s", hostList, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommParamsNeedSet))
		return
	}

	appID := hostList.ApplicationID
	lgc := logics.NewLogics(s.Engine, ctx.Kit.Header, s.CacheDB, s.AuthManager)
	if appID == 0 {
		// get default app id
		var err error
		appID, err = lgc.GetDefaultAppIDWithSupplier(ctx.Kit.Ctx)
		if err != nil {
			blog.Errorf("add host, but get default app id failed, err: %v,input:%+v,rid:%s", err, hostList, ctx.Kit.Rid)
			ctx.RespAutoError(err)
			return
		}
	}

	// 获取目标业务空先机模块ID
	cond := hutil.NewOperation().WithModuleName(common.DefaultResModuleName).WithAppID(appID).MapStr()
	cond.Set(common.BKDefaultField, common.DefaultResModuleFlag)
	moduleID, err := lgc.GetResourcePoolModuleID(ctx.Kit.Ctx, cond)
	if err != nil {
		blog.Errorf("add host, but get module id failed, err: %s,input: %+v,rid: %s", err.Error(), hostList, ctx.Kit.Rid)
		ctx.RespAutoError(err)
		return
	}

	retData := make(map[string]interface{})
	txnErr := s.Engine.CoreAPI.CoreService().Txn().AutoRunTxn(ctx.Kit.Ctx, s.EnableTxn, ctx.Kit.Header, func() error {
		_, success, updateErrRow, errRow, err := lgc.AddHost(ctx.Kit.Ctx, appID, []int64{moduleID},
			ctx.Kit.SupplierAccount, hostList.HostInfo, hostList.InputType)
		if err != nil {
			blog.Errorf("add host failed, success: %v, update: %v, err: %v, %v,input:%+v,rid:%s",
				success, updateErrRow, err, errRow, hostList, ctx.Kit.Rid)
			retData["error"] = errRow
			retData["update_error"] = updateErrRow
			return ctx.Kit.CCError.CCError(common.CCErrHostCreateFail)
		}
		retData["success"] = success
		return nil
	})

	if txnErr != nil {
		ctx.RespEntityWithError(retData, txnErr)
		return
	}
	ctx.RespEntity(retData)
}

// add host to resource pool, returns bk_host_id of the successfully added hosts
func (s *Service) AddHostToResourcePool(ctx *rest.Contexts) {

	hostList := new(meta.AddHostToResourcePoolHostList)
	body, err := ioutil.ReadAll(ctx.Request.Request.Body)
	if err != nil {
		blog.Errorf("read request body failed, err: %v, rid: %s", err, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommHTTPReadBodyFailed))
		return
	}
	if err := json.Unmarshal(body, hostList); err != nil {
		blog.Errorf("add host failed with decode body err: %v, body: %s, rid:%s", err, string(body), ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommJSONUnmarshalFailed))
		return
	}
	if hostList.HostInfo == nil {
		blog.ErrorJSON("add host, but host info is nil. input:%s, rid:%s", hostList, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommParamsNeedSet))
		return
	}
	lgc := logics.NewLogics(s.Engine, ctx.Kit.Header, s.CacheDB, s.AuthManager)
	_, retData, err := lgc.AddHostToResourcePool(ctx.Kit.Ctx, *hostList)

	if err != nil {
		blog.ErrorJSON("add host failed, retData: %s, err: %s, input:%s, rid:%s", retData, err, hostList, ctx.Kit.Rid)
		ctx.RespEntityWithError(retData, err)
		return
	}
	ctx.RespEntity(retData)
}

// Deprecated:
func (s *Service) AddHostFromAgent(ctx *rest.Contexts) {

	agents := new(meta.AddHostFromAgentHostList)
	if err := ctx.DecodeInto(&agents); nil != err {
		ctx.RespAutoError(err)
		return
	}

	if len(agents.HostInfo) == 0 {
		blog.Errorf("add host from agent, but got 0 agents from body.input:%+v,rid:%s", agents, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCErrorf(common.CCErrCommParamsNeedSet, "HostInfo"))
		return
	}
	lgc := logics.NewLogics(s.Engine, ctx.Kit.Header, s.CacheDB, s.AuthManager)
	appID, err := lgc.GetDefaultAppID(ctx.Kit.Ctx)
	if err != nil {
		blog.Errorf("AddHostFromAgent GetDefaultAppID error.input:%#v,rid:%s", agents, ctx.Kit.Rid)
		ctx.RespAutoError(err)
		return
	}
	if 0 == appID {
		blog.Errorf("add host from agent, but got invalid default appID, err: %v,ownerID:%s,input:%#v,rid:%s", err, ctx.Kit.SupplierAccount, agents, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCErrorf(common.CCErrAddHostToModule, "business not found"))
		return
	}

	opt := hutil.NewOperation().WithDefaultField(int64(common.DefaultResModuleFlag)).WithModuleName(common.DefaultResModuleName).WithAppID(appID)
	moduleID, err := lgc.GetResourcePoolModuleID(ctx.Kit.Ctx, opt.MapStr())
	if err != nil {
		blog.Errorf("add host from agent , but get module id failed, err: %v,ownerID:%s,input:%+v,rid:%s", err, ctx.Kit.SupplierAccount, agents, ctx.Kit.Rid)
		ctx.RespAutoError(err)
		return
	}

	agents.HostInfo["import_from"] = common.HostAddMethodAgent
	addHost := make(map[int64]map[string]interface{})
	addHost[1] = agents.HostInfo
	var success, updateErrRow, errRow []string
	retData := make(map[string]interface{})
	txnErr := s.Engine.CoreAPI.CoreService().Txn().AutoRunTxn(ctx.Kit.Ctx, s.EnableTxn, ctx.Kit.Header, func() error {
		var err error
		_, success, updateErrRow, errRow, err = lgc.AddHost(ctx.Kit.Ctx, appID, []int64{moduleID},
			common.BKDefaultOwnerID, addHost, "")
		if err != nil {
			blog.Errorf("add host failed, success: %v, update: %v, err: %v, %v,input:%+v,rid:%s",
				success, updateErrRow, err, errRow, agents, ctx.Kit.Rid)

			retData["success"] = success
			retData["error"] = errRow
			retData["update_error"] = updateErrRow
			return ctx.Kit.CCError.CCError(common.CCErrHostCreateFail)
		}

		return nil
	})

	if txnErr != nil {
		ctx.RespEntityWithError(retData, txnErr)
		return
	}
	ctx.RespEntity(success)
}

func (s *Service) SearchHost(ctx *rest.Contexts) {

	body := new(meta.HostCommonSearch)
	if err := ctx.DecodeInto(&body); nil != err {
		ctx.RespAutoError(err)
		return
	}

	lgc := logics.NewLogics(s.Engine, ctx.Kit.Header, s.CacheDB, s.AuthManager)
	host, err := lgc.SearchHost(ctx.Kit.Ctx, body, false)
	if err != nil {
		blog.Errorf("search host failed, err: %v,input:%+v,rid:%s", err, body, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrHostGetFail))
		return
	}

	hostIDArray := host.ExtractHostIDs()
	// auth: check authorization
	if err := s.AuthManager.AuthorizeByHostsIDs(ctx.Kit.Ctx, ctx.Kit.Header, authmeta.Find, *hostIDArray...); err != nil {
		blog.Errorf("check host authorization failed, hostID: %+v, err: %+v, rid: %s", hostIDArray, err, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommAuthorizeFailed))
		return
	}

	ctx.RespEntity(*host)

}

func (s *Service) SearchHostWithAsstDetail(ctx *rest.Contexts) {

	body := new(meta.HostCommonSearch)
	if err := ctx.DecodeInto(&body); nil != err {
		ctx.RespAutoError(err)
		return
	}

	lgc := logics.NewLogics(s.Engine, ctx.Kit.Header, s.CacheDB, s.AuthManager)
	host, err := lgc.SearchHost(ctx.Kit.Ctx, body, true)
	if err != nil {
		blog.Errorf("search host failed, err: %v,input:%+v,rid:%s", err, body, ctx.Kit.Rid)
		ctx.RespAutoError(err)
		return
	}

	// auth: check authorization
	hostIDArray := host.ExtractHostIDs()
	if err := s.AuthManager.AuthorizeByHostsIDs(ctx.Kit.Ctx, ctx.Kit.Header, authmeta.Find, *hostIDArray...); err != nil {
		blog.Errorf("check host authorization failed, hosts: %+v, err: %v, rid: %s", hostIDArray, err, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommAuthorizeFailed))
		return
	}

	ctx.RespEntity(*host)
}

func (s *Service) getHostApplyRelatedFields(ctx *rest.Contexts, hostIDArr []int64) (hostProperties map[int64][]string, hasRules bool, ccErr errors.CCErrorCoder) {
	// filter fields locked by host apply rule
	listRuleOption := meta.ListHostRelatedApplyRuleOption{
		HostIDs: hostIDArr,
		Page: meta.BasePage{
			Limit: common.BKNoLimit,
		},
	}
	hostRules, ccErr := s.listHostRelatedApplyRule(ctx, 0, listRuleOption)
	if ccErr != nil {
		blog.Errorf("update host batch, listHostRelatedApplyRule failed, option: %+v, err: %v, rid: %s", listRuleOption, ccErr, ctx.Kit.Rid)
		return nil, false, ccErr
	}
	attributeIDs := make([]int64, 0)
	for _, rules := range hostRules {
		for _, rule := range rules {
			attributeIDs = append(attributeIDs, rule.AttributeID)
		}
	}
	if len(attributeIDs) == 0 {
		return nil, false, nil
	}
	hostAttributesFilter := &meta.QueryCondition{
		Fields: []string{common.BKPropertyIDField, common.BKFieldID},
		Page: meta.BasePage{
			Limit: common.BKNoLimit,
		},
		Condition: map[string]interface{}{
			common.BKFieldID: map[string]interface{}{
				common.BKDBIN: attributeIDs,
			},
		},
	}
	attributeResult, err := s.CoreAPI.CoreService().Model().ReadModelAttr(ctx.Kit.Ctx, ctx.Kit.Header, common.BKInnerObjIDHost, hostAttributesFilter)
	if err != nil {
		blog.Errorf("UpdateHostBatch failed, ReadModelAttr failed, param: %+v, err: %+v, rid:%s", hostAttributesFilter, err, ctx.Kit.Rid)
		return nil, true, ctx.Kit.CCError.CCError(common.CCErrCommHTTPDoRequestFailed)
	}
	if ccErr := attributeResult.CCError(); ccErr != nil {
		blog.Errorf("UpdateHostBatch failed, ReadModelAttr failed, param: %+v, output: %+v, rid:%s", hostAttributesFilter, attributeResult, ctx.Kit.Rid)
		return nil, true, ccErr
	}
	attributeMap := make(map[int64]meta.Attribute)
	for _, item := range attributeResult.Data.Info {
		attributeMap[item.ID] = item
	}
	hostProperties = make(map[int64][]string)
	for hostID, rules := range hostRules {
		if _, exist := hostProperties[hostID]; exist == false {
			hostProperties[hostID] = make([]string, 0)
		}
		for _, rule := range rules {
			attribute, ok := attributeMap[rule.AttributeID]
			if ok == false {
				continue
			}
			hostProperties[hostID] = append(hostProperties[hostID], attribute.PropertyID)
		}
	}
	return hostProperties, true, nil
}

func (s *Service) UpdateHostBatch(ctx *rest.Contexts) {

	data := mapstr.New()
	if err := ctx.DecodeInto(&data); nil != err {
		ctx.RespAutoError(err)
		return
	}

	// TODO: this is a wrong usage, just for compatible the wrong usage before.
	// delete this, when the frontend use the rigListHostInstanceht request field. not the number.
	id := data[common.BKHostIDField]
	hostIDStr := ""
	switch id.(type) {
	case float64:
		floatID := id.(float64)
		hostIDStr = strconv.FormatInt(int64(floatID), 10)
	case string:
		hostIDStr = id.(string)
	default:
		blog.Errorf("update host batch failed, got invalid host id(%v) data type,rid:%s", id, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCErrorf(common.CCErrCommParamsIsInvalid, "bk_host_id"))
		return
	}

	data.Remove(common.MetadataField)
	data.Remove(common.BKHostIDField)
	data.Remove(common.BKCloudIDField)

	lgc := logics.NewLogics(s.Engine, ctx.Kit.Header, s.CacheDB, s.AuthManager)
	hostFields, err := lgc.GetHostAttributes(ctx.Kit.Ctx, ctx.Kit.SupplierAccount, meta.BizLabelNotExist)

	if err != nil {
		blog.Errorf("update host batch, but get host attribute for audit failed, err: %v,rid:%s", err, ctx.Kit.Rid)
		ctx.RespAutoError(err)
		return
	}

	// check authorization
	hostIDArr := make([]int64, 0)
	for _, id := range strings.Split(hostIDStr, ",") {
		hostID, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			blog.Errorf("update host batch, but got invalid host id[%s], err: %v,rid:%s", id, err, ctx.Kit.Rid)
			ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommParamsInvalid))
			return
		}
		hostIDArr = append(hostIDArr, hostID)
	}
	// auth: check authorization
	if err := s.AuthManager.AuthorizeByHostsIDs(ctx.Kit.Ctx, ctx.Kit.Header, authmeta.Update, hostIDArr...); err != nil {
		blog.Errorf("check host authorization failed, hosts: %+v, err: %v, rid: %s", hostIDArr, err, ctx.Kit.Rid)
		if err != nil && err != ac.NoAuthorizeError {
			blog.ErrorJSON("check host authorization failed, hosts: %s, err: %s, rid: %s", hostIDArr, err.Error(), ctx.Kit.Rid)
			ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommAuthorizeFailed))
			return
		}
		perm, err := s.AuthManager.GenHostBatchNoPermissionResp(ctx.Kit.Ctx, ctx.Kit.Header, authmeta.Update, hostIDArr)
		if err != nil && err != ac.NoAuthorizeError {
			blog.ErrorJSON("check host authorization get permission failed, hosts: %s, err: %s, rid: %s", hostIDArr, err.Error(), ctx.Kit.Rid)
			ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommAuthorizeFailed))
			return
		}
		ctx.RespEntityWithError(perm, ac.NoAuthorizeError)
		return
	}

	logPreContents := make(map[int64]*logics.HostLog, 0)
	for _, hostID := range hostIDArr {
		audit := lgc.NewHostLog(ctx.Kit.Ctx, ctx.Kit.SupplierAccount)
		if err := audit.WithPrevious(ctx.Kit.Ctx, hostID, hostFields); err != nil {
			blog.Errorf("update host batch, but get host[%s] pre data for audit failed, err: %v, rid: %s", id, err, ctx.Kit.Rid)
			ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrHostDetailFail))
			return
		}

		logPreContents[hostID] = audit
	}

	txnErr := s.Engine.CoreAPI.CoreService().Txn().AutoRunTxn(ctx.Kit.Ctx, s.EnableTxn, ctx.Kit.Header, func() error {
		hasHostUpdateWithoutHostApplyFiled := false
		// 功能开关：更新主机属性时是否剔除自动应用字段
		if meta.HostUpdateWithoutHostApplyFiled == true {
			hostProperties, hasRules, err := s.getHostApplyRelatedFields(ctx, hostIDArr)
			if err != nil {
				blog.Errorf("UpdateHostBatch failed, getHostApplyRelatedFields failed, hostIDArr: %+v, err: %v, rid:%s", hostIDArr, err, ctx.Kit.Rid)
				return err
			}
			// get host attributes
			if hasRules == true {
				hasHostUpdateWithoutHostApplyFiled = true
				for _, hostID := range hostIDArr {
					updateData := make(map[string]interface{})
					for key, value := range data {
						properties, ok := hostProperties[hostID]
						if ok == true && util.InStrArr(properties, key) {
							continue
						}
						updateData[key] = value
					}
					opt := &meta.UpdateOption{
						Condition: mapstr.MapStr{common.BKHostIDField: hostID},
						Data:      mapstr.NewFromMap(updateData),
					}
					result, err := s.CoreAPI.CoreService().Instance().UpdateInstance(ctx.Kit.Ctx, ctx.Kit.Header, common.BKInnerObjIDHost, opt)
					if err != nil {
						blog.Errorf("UpdateHostBatch UpdateObject http do error, err: %v,input:%+v,param:%+v,rid:%s", err, data, opt, ctx.Kit.Rid)
						return ctx.Kit.CCError.CCError(common.CCErrCommHTTPDoRequestFailed)
					}
					if !result.Result {
						blog.ErrorJSON("UpdateHostBatch failed, UpdateObject failed, param:%s, response: %s, rid:%s", opt, result, ctx.Kit.Rid)
						return result.CCError()
					}
				}
			}
		}

		if hasHostUpdateWithoutHostApplyFiled == false {
			// 退化到批量编辑
			opt := &meta.UpdateOption{
				Condition: mapstr.MapStr{common.BKHostIDField: mapstr.MapStr{common.BKDBIN: hostIDArr}},
				Data:      mapstr.NewFromMap(data),
			}
			result, err := s.CoreAPI.CoreService().Instance().UpdateInstance(ctx.Kit.Ctx, ctx.Kit.Header, common.BKInnerObjIDHost, opt)
			if err != nil {
				blog.Errorf("UpdateHostBatch UpdateObject http do error, err: %v,input:%+v,param:%+v,rid:%s", err, data, opt, ctx.Kit.Rid)
				return ctx.Kit.CCError.CCError(common.CCErrCommHTTPDoRequestFailed)
			}
			if !result.Result {
				blog.ErrorJSON("UpdateHostBatch failed, UpdateObject failed, param:%s, response: %s, rid:%s", opt, result, ctx.Kit.Rid)
				return result.CCError()
			}
		}

		hostModuleConfig, err := lgc.GetConfigByCond(ctx.Kit.Ctx, meta.HostModuleRelationRequest{HostIDArr: hostIDArr, Fields: []string{common.BKAppIDField, common.BKHostIDField}})
		if err != nil {
			blog.Errorf("update host batch GetConfigByCond failed, hostIDArr[%v], err: %v,input:%+v,rid:%s", hostIDArr, err, data, ctx.Kit.Rid)
			return err
		}
		appIDMap := make(map[int64]int64)
		for _, hostModule := range hostModuleConfig {
			appIDMap[hostModule.HostID] = hostModule.AppID
		}

		logLastContents := make([]meta.AuditLog, 0)
		for _, hostID := range hostIDArr {
			audit, ok := logPreContents[hostID]
			if !ok {
				audit = lgc.NewHostLog(ctx.Kit.Ctx, common.BKDefaultOwnerID)
			}
			if err := audit.WithCurrent(ctx.Kit.Ctx, hostID, hostFields); err != nil {
				blog.Errorf("update host batch, but get host[%v] pre data for audit failed, err: %v, rid: %s", hostID, err, ctx.Kit.Rid)
				return ctx.Kit.CCError.CCError(common.CCErrHostDetailFail)
			}
			auditLog, err := audit.AuditLog(ctx.Kit.Ctx, hostID, appIDMap[hostID], meta.AuditUpdate)
			if err != nil {
				blog.Errorf("update host batch, but get host[%v] biz[%v] data for audit failed, err: %v, rid: %s", hostID, appIDMap[hostID], err, ctx.Kit.Rid)
				return err
			}
			logLastContents = append(logLastContents, auditLog)
		}
		auditResp, err := s.CoreAPI.CoreService().Audit().SaveAuditLog(ctx.Kit.Ctx, ctx.Kit.Header, logLastContents...)
		if err != nil {
			blog.Errorf("update host property batch, but add host[%v] audit failed, err: %v, rid:%s", hostIDArr, err, ctx.Kit.Rid)
			return err
		}
		if !auditResp.Result {
			blog.Errorf("update host property batch, but add host[%v] audit failed, err: %v, rid:%s", hostIDArr, auditResp.ErrMsg, ctx.Kit.Rid)
			return auditResp.CCError()
		}
		return nil
	})

	if txnErr != nil {
		ctx.RespAutoError(txnErr)
		return
	}
	ctx.RespEntity(nil)
}

func (s *Service) UpdateHostPropertyBatch(ctx *rest.Contexts) {

	parameter := new(meta.UpdateHostPropertyBatchParameter)
	if err := ctx.DecodeInto(&parameter); nil != err {
		ctx.RespAutoError(err)
		return
	}

	if len(parameter.Update) > common.BKMaxPageSize {
		blog.Errorf("UpdateHostPropertyBatch failed, data len %d exceed max pageSize %d, rid:%s", len(parameter.Update), common.BKMaxPageSize, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCErrorf(common.CCErrCommXXExceedLimit, "update", common.BKMaxPageSize))
		return
	}

	lgc := logics.NewLogics(s.Engine, ctx.Kit.Header, s.CacheDB, s.AuthManager)
	hostFields, err := lgc.GetHostAttributes(ctx.Kit.Ctx, ctx.Kit.SupplierAccount, meta.BizLabelNotExist)
	if err != nil {
		blog.Errorf("update host property batch, but get host attribute for audit failed, err: %v,rid:%s", err, ctx.Kit.Rid)
		ctx.RespAutoError(err)
		return
	}

	// check authorization
	hostIDArr := make([]int64, 0)
	for _, update := range parameter.Update {
		hostIDArr = append(hostIDArr, update.HostID)
	}
	// auth: check authorization
	if err := s.AuthManager.AuthorizeByHostsIDs(ctx.Kit.Ctx, ctx.Kit.Header, authmeta.Update, hostIDArr...); err != nil {
		blog.Errorf("check host authorization failed, hosts: %+v, err: %v, rid: %s", hostIDArr, err, ctx.Kit.Rid)
		if err != nil && err != ac.NoAuthorizeError {
			blog.ErrorJSON("check host authorization failed, hosts: %s, err: %s, rid: %s", hostIDArr, err.Error(), ctx.Kit.Rid)
			ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommAuthorizeFailed))
			return
		}
		perm, err := s.AuthManager.GenHostBatchNoPermissionResp(ctx.Kit.Ctx, ctx.Kit.Header, authmeta.Update, hostIDArr)
		if err != nil && err != ac.NoAuthorizeError {
			blog.ErrorJSON("check host authorization get permission failed, hosts: %s, err: %s, rid: %s", hostIDArr, err.Error(), ctx.Kit.Rid)
			ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommAuthorizeFailed))
			return
		}
		ctx.RespEntityWithError(perm, ac.NoAuthorizeError)

		return
	}

	txnErr := s.Engine.CoreAPI.CoreService().Txn().AutoRunTxn(ctx.Kit.Ctx, s.EnableTxn, ctx.Kit.Header, func() error {
		auditLogs := make([]meta.AuditLog, 0)
		for _, update := range parameter.Update {
			cond := mapstr.New()
			cond.Set(common.BKHostIDField, update.HostID)
			data, err := mapstr.NewFromInterface(update.Properties)
			if err != nil {
				blog.Errorf("update host property batch, but convert properties[%v] to mapstr failed, err: %v, rid: %s", update.Properties, err, ctx.Kit.Rid)
				return err
			}
			// can't update host's cloud area using this api
			data.Remove(common.BKCloudIDField)
			data.Remove(common.BKHostIDField)
			opt := &meta.UpdateOption{
				Condition: cond,
				Data:      data,
			}
			hostLog := lgc.NewHostLog(ctx.Kit.Ctx, ctx.Kit.SupplierAccount)
			if err := hostLog.WithPrevious(ctx.Kit.Ctx, update.HostID, hostFields); err != nil {
				blog.Errorf("update host property batch, but get host[%d] pre data for audit failed, err: %v, rid: %s", update.HostID, err, ctx.Kit.Rid)
				return err
			}
			result, err := s.CoreAPI.CoreService().Instance().UpdateInstance(ctx.Kit.Ctx, ctx.Kit.Header, common.BKInnerObjIDHost, opt)
			if err != nil {
				blog.Errorf("UpdateHostPropertyBatch UpdateInstance http do error, err: %v,input:%+v,param:%+v,rid:%s", err, data, opt, ctx.Kit.Rid)
				return err
			}
			if !result.Result {
				blog.Errorf("UpdateHostPropertyBatch UpdateObject http response error, err code:%d,err msg:%s,input:%+v,param:%+v,rid:%s", result.Code, data, opt, ctx.Kit.Rid)
				return result.CCError()
			}

			if err := hostLog.WithCurrent(ctx.Kit.Ctx, update.HostID, nil); err != nil {
				blog.Errorf("update host property batch, but get host[%d] pre data for audit failed, err: %v, rid: %s", update.HostID, err, ctx.Kit.Rid)
				return err
			}

			hostModuleConfig, err := lgc.GetConfigByCond(ctx.Kit.Ctx, meta.HostModuleRelationRequest{HostIDArr: []int64{update.HostID}, Fields: []string{common.BKAppIDField}})
			if err != nil {
				blog.Errorf("update host property batch GetConfigByCond failed, hostID[%v], err: %v,rid:%s", update.HostID, err, ctx.Kit.Rid)
				return err
			}
			var appID int64
			if len(hostModuleConfig) > 0 {
				appID = hostModuleConfig[0].AppID
			}
			auditLog, err := hostLog.AuditLog(ctx.Kit.Ctx, update.HostID, appID, meta.AuditUpdate)
			if err != nil {
				blog.Errorf("update host property batch, but get host[%d] biz[%d] data for audit failed, err: %v, rid: %s", update.HostID, appID, err, ctx.Kit.Rid)
				return err
			}
			auditLogs = append(auditLogs, auditLog)
		}

		auditResp, err := s.CoreAPI.CoreService().Audit().SaveAuditLog(ctx.Kit.Ctx, ctx.Kit.Header, auditLogs...)
		if err != nil {
			blog.Errorf("update host property batch, but add host[%v] audit failed, err: %v, rid:%s", hostIDArr, err, ctx.Kit.Rid)
			return ctx.Kit.CCError.CCError(common.CCErrCommHTTPDoRequestFailed)
		}
		if !auditResp.Result {
			blog.Errorf("update host property batch, but add host[%v] audit failed, err: %v, rid:%s", hostIDArr, auditResp.ErrMsg, ctx.Kit.Rid)
			return auditResp.CCError()
		}
		return nil
	})

	if txnErr != nil {
		ctx.RespAutoError(txnErr)
		return
	}
	ctx.RespEntity(nil)
}

// NewHostSyncAppTopo add new hosts to the business
// synchronize hosts directly to a module in a business if this host does not exist.
// otherwise, this operation will only change host's attribute.
// TODO: used by framework.
func (s *Service) NewHostSyncAppTopo(ctx *rest.Contexts) {

	hostList := new(meta.HostSyncList)
	if err := ctx.DecodeInto(&hostList); nil != err {
		ctx.RespAutoError(err)
		return
	}

	if hostList.HostInfo == nil {
		blog.Errorf("add host, but host info is nil.input:%+v,rid:%s", hostList, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCErrorf(common.CCErrCommParamsNeedSet, "host_info"))
		return
	}
	if 0 == len(hostList.ModuleID) {
		blog.Errorf("host sync app  parameters required moduleID,input:%+v,rid:%s", hostList, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCErrorf(common.CCErrCommParamsNeedSet, common.BKModuleIDField))
		return
	}

	if common.BatchHostAddMaxRow < len(hostList.HostInfo) {
		ctx.RespAutoError(ctx.Kit.CCError.CCErrorf(common.CCErrCommXXExceedLimit, "host_info ", common.BatchHostAddMaxRow))
		return
	}

	appConds := map[string]interface{}{
		common.BKAppIDField: hostList.ApplicationID,
	}

	lgc := logics.NewLogics(s.Engine, ctx.Kit.Header, s.CacheDB, s.AuthManager)
	appInfo, err := lgc.GetAppDetails(ctx.Kit.Ctx, "", appConds)
	if nil != err {
		blog.Errorf("host sync app %d error:%s,input:%+v,rid:%s", hostList.ApplicationID, err.Error(), hostList, ctx.Kit.Rid)
		ctx.RespAutoError(err)
		return
	}
	if 0 == len(appInfo) {
		blog.Errorf("host sync app %d not found, reply:%+v,input:%+v,rid:%s", hostList.ApplicationID, appInfo, hostList, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrTopoGetAppFailed))
		return
	}

	moduleCond := []meta.ConditionItem{
		{
			Field:    common.BKModuleIDField,
			Operator: common.BKDBIN,
			Value:    hostList.ModuleID,
		},
	}
	if len(hostList.ModuleID) > 1 {
		moduleCond = append(moduleCond, meta.ConditionItem{
			Field:    common.BKDefaultField,
			Operator: common.BKDBEQ,
			Value:    common.DefaultFlagDefaultValue,
		})
	}
	// srvData.lgc..NewHostSyncValidModule(req, data.ApplicationID, data.ModuleID, m.CC.ObjCtrl())
	moduleIDS, err := lgc.GetModuleIDByCond(ctx.Kit.Ctx, moduleCond)
	if nil != err {
		blog.Errorf("NewHostSyncAppTop GetModuleIDByCond error. err:%s,input:%+v,rid:%s", err.Error(), hostList, ctx.Kit.Rid)
		ctx.RespAutoError(err)
		return
	}
	if len(moduleIDS) != len(hostList.ModuleID) {
		blog.Errorf("not found part module: source:%v, db:%v, rid: %s", hostList.ModuleID, moduleIDS, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCErrorf(common.CCErrHostModuleIDNotFoundORHasMultipleInnerModuleIDFailed))
		return
	}

	// auth: check authorization
	if err := s.AuthManager.AuthorizeCreateHost(ctx.Kit.Ctx, ctx.Kit.Header, hostList.ApplicationID); err != nil {
		blog.Errorf("check add hosts authorization failed, business: %d, err: %v, rid: %s", hostList.ApplicationID, err, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommAuthorizeFailed))
		return
	}

	retData := make(map[string]interface{})
	var success, updateErrRow, errRow []string
	txnErr := s.Engine.CoreAPI.CoreService().Txn().AutoRunTxn(ctx.Kit.Ctx, s.EnableTxn, ctx.Kit.Header, func() error {
		var err error
		_, success, updateErrRow, errRow, err = lgc.AddHost(ctx.Kit.Ctx, hostList.ApplicationID,
			hostList.ModuleID, ctx.Kit.SupplierAccount, hostList.HostInfo, common.InputTypeApiNewHostSync)
		if err != nil {
			blog.Errorf("add host failed, success: %v, update: %v, err: %v, %v, rid: %s",
				success, updateErrRow, err, errRow, ctx.Kit.Rid)

			retData["success"] = success
			retData["error"] = errRow
			retData["update_error"] = updateErrRow
			return ctx.Kit.CCError.CCError(common.CCErrHostCreateFail)
		}

		return nil
	})

	if txnErr != nil {
		ctx.RespEntityWithError(retData, txnErr)
		return
	}
	ctx.RespEntity(success)
}

// MoveSetHost2IdleModule bk_set_id and bk_module_id cannot be empty at the same time
// Remove the host from the module or set.
// The host belongs to the current module or host only, and puts the host into the idle machine of the current service.
// When the host data is in multiple modules or sets. Disconnect the host from the module or set only
// TODO: used by v2 version, remove this api when v2 is offline.
func (s *Service) MoveSetHost2IdleModule(ctx *rest.Contexts) {
	header := ctx.Kit.Header

	var data meta.SetHostConfigParams
	if err := ctx.DecodeInto(&data); nil != err {
		ctx.RespAutoError(err)
		return
	}

	if 0 == data.ApplicationID {
		blog.Errorf("MoveSetHost2IdleModule bk_biz_id cannot be empty at the same time,input:%#v,rid:%s", data, util.GetHTTPCCRequestID(header))
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommParamsNeedSet))
		return
	}

	if 0 == data.SetID && 0 == data.ModuleID {
		blog.Errorf("MoveSetHost2IdleModule bk_set_id and bk_module_id cannot be empty at the same time,input:%#v,rid:%s", data, util.GetHTTPCCRequestID(header))
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommParamsNeedSet))
		return
	}

	// get host in set
	condition := &meta.DistinctHostIDByTopoRelationRequest{}

	if 0 != data.SetID {
		condition.SetIDArr = []int64{data.SetID}
	}
	if 0 != data.ModuleID {
		condition.ModuleIDArr = []int64{data.ModuleID}
	}

	condition.ApplicationIDArr = []int64{data.ApplicationID}
	lgc := logics.NewLogics(s.Engine, header, s.CacheDB, s.AuthManager)
	hostResult, err := lgc.CoreAPI.CoreService().Host().GetDistinctHostIDByTopology(ctx.Kit.Ctx, header, condition)
	if err != nil {
		blog.Errorf("get host ids failed, err: %v, rid: %s", err, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommHTTPDoRequestFailed))
		return
	}
	if err := hostResult.CCError(); err != nil {
		blog.ErrorJSON("get host id by topology relation failed, error code: %s, error message: %s, cond: %s, rid: %s", hostResult.Code, hostResult.ErrMsg, condition, ctx.Kit.Rid)
		ctx.RespAutoError(err)
		return
	}

	hostIDArr := hostResult.Data.IDArr
	if 0 == len(hostIDArr) {
		blog.Warnf("no host in set,rid:%s", ctx.Kit.Rid)
		ctx.RespEntity(nil)
		return
	}
	moduleCond := []meta.ConditionItem{
		{
			Field:    common.BKAppIDField,
			Operator: common.BKDBEQ,
			Value:    data.ApplicationID,
		},
		{
			Field:    common.BKDefaultField,
			Operator: common.BKDBEQ,
			Value:    common.DefaultResModuleFlag,
		},
	}

	moduleIDArr, err := lgc.GetModuleIDByCond(ctx.Kit.Ctx, moduleCond)
	if err != nil {
		blog.Errorf("MoveSetHost2IdleModule GetModuleIDByCond error. err:%s, input:%#v, param:%#v, rid:%s", err.Error(), data, moduleCond, ctx.Kit.Rid)
		ctx.RespAutoError(err)
		return
	}
	if len(moduleIDArr) == 0 {
		blog.Errorf("MoveSetHost2IdleModule GetModuleIDByCond idle module not exist, input:%#v, param:%#v, rid:%s", data, moduleCond, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCErrorf(common.CCErrHostModuleNotExist, "idle module"))
		return
	}
	idleModuleID := moduleIDArr[0]
	moduleHostConfigParams := make(map[string]interface{})
	moduleHostConfigParams[common.BKAppIDField] = data.ApplicationID
	audit := lgc.NewHostModuleLog(hostIDArr)

	var exceptionArr []meta.ExceptionResult
	txnErr := s.Engine.CoreAPI.CoreService().Txn().AutoRunTxn(ctx.Kit.Ctx, s.EnableTxn, ctx.Kit.Header, func() error {

		hmInput := &meta.HostModuleRelationRequest{
			ApplicationID: data.ApplicationID,
			HostIDArr:     hostIDArr,
			Fields:        []string{common.BKSetIDField, common.BKModuleIDField, common.BKHostIDField},
		}
		configResult, err := lgc.CoreAPI.CoreService().Host().GetHostModuleRelation(ctx.Kit.Ctx, ctx.Kit.Header, hmInput)
		if nil != err {
			blog.Errorf("remove hostModuleConfig, http do error, error:%v, params:%v, input:%+v, rid:%s", err, hmInput, data, ctx.Kit.Rid)
			return err
		}
		if !configResult.Result {
			blog.Errorf("remove hostModuleConfig http reply error, result:%v, params:%v, input:%+v, rid:%s", configResult, hmInput, data, ctx.Kit.Rid)
			return err
		}
		hostIDMHMap := make(map[int64][]meta.ModuleHost, 0)
		for _, item := range configResult.Data.Info {
			hostIDMHMap[item.HostID] = append(hostIDMHMap[item.HostID], item)
		}

		for _, hostID := range hostIDArr {
			hostMHArr, ok := hostIDMHMap[hostID]
			if !ok {
				// ignore  not exist the host under the current business,
				continue
			}
			toEmptyModule := true
			var newModuleIDArr []int64
			for _, item := range hostMHArr {
				if 0 != data.ModuleID && item.ModuleID == data.ModuleID {
					continue
				}
				if 0 != data.SetID && 0 == data.ModuleID && item.SetID == data.SetID {
					continue
				}

				toEmptyModule = false
				newModuleIDArr = append(newModuleIDArr, item.ModuleID)
			}

			var opResult *meta.OperaterException
			if toEmptyModule {
				input := &meta.TransferHostToInnerModule{
					ApplicationID: data.ApplicationID,
					ModuleID:      idleModuleID,
					HostID:        []int64{hostID},
				}
				opResult, err = lgc.CoreAPI.CoreService().Host().TransferToInnerModule(ctx.Kit.Ctx, ctx.Kit.Header, input)
			} else {
				input := &meta.HostsModuleRelation{
					ApplicationID: data.ApplicationID,
					HostID:        []int64{hostID},
					ModuleID:      newModuleIDArr,
				}
				opResult, err = lgc.CoreAPI.CoreService().Host().TransferToNormalModule(ctx.Kit.Ctx, ctx.Kit.Header, input)
			}

			if err != nil {
				blog.Errorf("MoveSetHost2IdleModule handle error. err:%s, to idle module:%v, input:%#v, hostID:%d, rid:%s", err.Error(), toEmptyModule, data, hostID, ctx.Kit.Rid)
				ccErr := ctx.Kit.CCError.CCError(common.CCErrCommHTTPDoRequestFailed)
				exceptionArr = append(exceptionArr, meta.ExceptionResult{Code: int64(ccErr.GetCode()), Message: ccErr.Error(), OriginIndex: hostID})
			}
			if !opResult.Result {
				if len(opResult.Data) > 0 {
					blog.Errorf("MoveSetHost2IdleModule handle reply error. result:%#v, to idle module:%v, input:%#v, hostID:%d, rid:%s", opResult, toEmptyModule, data, hostID, ctx.Kit.Rid)
					exceptionArr = append(exceptionArr, opResult.Data...)
				} else {
					blog.Errorf("MoveSetHost2IdleModule handle reply error. result:%#v, to idle module:%v, input:%#v, hostID:%d, rid:%s", opResult, toEmptyModule, data, hostID, ctx.Kit.Rid)
					exceptionArr = append(exceptionArr, meta.ExceptionResult{
						Code:        int64(opResult.Code),
						Message:     opResult.ErrMsg,
						OriginIndex: hostID,
					})
				}
			}
		}

		if err := audit.SaveAudit(ctx.Kit.Ctx); err != nil {
			blog.Errorf("SaveAudit failed, err: %s, rid: %s", err.Error(), ctx.Kit.Rid)
			return ctx.Kit.CCError.CCError(common.CCErrHostDeleteFail)
		}

		if len(exceptionArr) > 0 {
			blog.Errorf("MoveSetHost2IdleModule has exception. exception:%#v, rid:%s", exceptionArr, ctx.Kit.Rid)
			return ctx.Kit.CCError.CCError(common.CCErrHostDeleteFail)
		}
		return nil
	})

	if txnErr != nil {
		ctx.RespEntityWithError(exceptionArr, txnErr)
		return
	}
	ctx.RespEntity(nil)
}

func (s *Service) ip2hostID(ctx *rest.Contexts, ip string, cloudID int64) (hostID int64, err error) {
	lgc := logics.NewLogics(s.Engine, ctx.Kit.Header, s.CacheDB, s.AuthManager)
	_, hostID, err = lgc.IPCloudToHost(ctx.Kit.Ctx, ip, cloudID)
	return hostID, err
}

// CloneHostProperty clone host property from src host to dst host
func (s *Service) CloneHostProperty(ctx *rest.Contexts) {

	input := &meta.CloneHostPropertyParams{}
	if err := ctx.DecodeInto(&input); nil != err {
		ctx.RespAutoError(err)
		return
	}

	if input.OrgIP == input.DstIP {
		ctx.RespEntity(nil)
		return
	}

	if 0 == input.AppID {
		blog.Errorf("CloneHostProperty, application not found input:%+v,rid:%s", input, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCErrorf(common.CCErrCommParamsNeedInt, "ApplicationID"))
		return
	}
	if input.OrgIP == "" {
		blog.Errorf("CloneHostProperty, OrgIP not found input:%+v,rid:%s", input, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCErrorf(common.CCErrCommParamsNeedSet, "bk_org_ip"))
		return
	}
	if input.DstIP == "" {
		blog.Errorf("CloneHostProperty, OrgIP not found input:%+v,rid:%s", input, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCErrorf(common.CCErrCommParamsNeedSet, "bk_dst_ip"))
		return
	}

	// authorization check
	srcHostID, err := s.ip2hostID(ctx, input.OrgIP, input.CloudID)
	if err != nil {
		blog.Errorf("ip2hostID failed, ip:%s, input:%+v, rid:%s", input.OrgIP, input, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCErrorf(common.CCErrCommParamsNeedInt, "OrgIP"))
		return
	}
	// check source host exist
	if srcHostID == 0 {
		blog.Errorf("host not found. params:%s,rid:%s", input, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrHostNotFound))
		return
	}
	// auth: check authorization
	if err := s.AuthManager.AuthorizeByHostsIDs(ctx.Kit.Ctx, ctx.Kit.Header, authmeta.Find, srcHostID); err != nil {
		blog.Errorf("check host authorization failed, hosts: %+v, err: %v, rid:%s", srcHostID, err, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommAuthorizeFailed))
		return
	}
	// step2. verify has permission to update dst host
	dstHostID, err := s.ip2hostID(ctx, input.DstIP, input.CloudID)
	if err != nil {
		blog.Errorf("ip2hostID failed, ip:%s, input:%+v, rid:%s", input.DstIP, input, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCErrorf(common.CCErrCommParamsNeedInt, "DstIP"))
		return
	}
	// check whether destination host exist
	if dstHostID == 0 {
		blog.Errorf("host not found. params:%s,rid:%s", input, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrHostNotFound))
		return
	}

	// auth: check authorization
	if err := s.AuthManager.AuthorizeByHostsIDs(ctx.Kit.Ctx, ctx.Kit.Header, authmeta.Update, dstHostID); err != nil {
		if err != ac.NoAuthorizeError {
			blog.Errorf("check host authorization failed, hosts: %+v, err: %v, rid:%s", dstHostID, err, ctx.Kit.Rid)
			ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommAuthorizeFailed))
			return
		}
		perm, err := s.AuthManager.GenEditBizHostNoPermissionResp(ctx.Kit.Ctx, ctx.Kit.Header, []int64{dstHostID})
		if err != nil {
			ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommAuthorizeFailed))
			return
		}
		ctx.RespEntityWithError(perm, ac.NoAuthorizeError)
		return
	}
	lgc := logics.NewLogics(s.Engine, ctx.Kit.Header, s.CacheDB, s.AuthManager)
	txnErr := s.Engine.CoreAPI.CoreService().Txn().AutoRunTxn(ctx.Kit.Ctx, s.EnableTxn, ctx.Kit.Header, func() error {
		err = lgc.CloneHostProperty(ctx.Kit.Ctx, input.AppID, srcHostID, dstHostID)
		if nil != err {
			blog.Errorf("CloneHostProperty  error , err: %v, input:%#v, rid:%s", err, input, ctx.Kit.Rid)
			return err
		}
		return nil
	})

	if txnErr != nil {
		ctx.RespAutoError(txnErr)
		return
	}
	ctx.RespEntity(nil)
}

// UpdateImportHosts update excel import hosts
func (s *Service) UpdateImportHosts(ctx *rest.Contexts) {
	hostList := new(meta.HostList)
	if err := ctx.DecodeInto(&hostList); nil != err {
		ctx.RespAutoError(err)
		return
	}

	if hostList.HostInfo == nil {
		blog.Errorf("UpdateImportHosts, but host info is nil.input:%+v,rid:%s", hostList, ctx.Kit.Rid)
		ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommParamsNeedSet))
		return
	}
	lgc := logics.NewLogics(s.Engine, ctx.Kit.Header, s.CacheDB, s.AuthManager)
	hostFields, err := lgc.GetHostAttributes(ctx.Kit.Ctx, ctx.Kit.SupplierAccount, meta.BizLabelNotExist)
	if err != nil {
		blog.Errorf("UpdateImportHosts, but get host attribute for audit failed, err: %v,rid:%s", err, ctx.Kit.Rid)
		ctx.RespAutoError(err)
		return
	}

	hostIDArr := make([]int64, 0)
	hosts := make(map[int64]map[string]interface{}, 0)
	indexHostIDMap := make(map[int64]int64, 0)
	var errMsg, successMsg []string
	CCLang := s.Language.CreateDefaultCCLanguageIf(util.GetLanguage(ctx.Kit.Header))
	for index, hostInfo := range hostList.HostInfo {
		if hostInfo == nil {
			continue
		}
		var intHostID int64
		hostID, ok := hostInfo[common.BKHostIDField]
		if !ok {
			blog.Errorf("UpdateImportHosts failed, because bk_host_id field not exits innerIp: %v, rid: %v", hostInfo[common.BKHostInnerIPField], ctx.Kit.Rid)

			errMsg = append(errMsg, CCLang.Languagef("import_update_host_miss_hostID", index))
			continue
		}
		intHostID, err = util.GetInt64ByInterface(hostID)
		if err != nil {
			errMsg = append(errMsg, CCLang.Languagef("import_update_host_hostID_not_int", index))
			continue
		}
		// bk_host_innerip should not update
		delete(hostInfo, common.BKHostInnerIPField)
		hostIDArr = append(hostIDArr, intHostID)
		hosts[index] = hostInfo
		indexHostIDMap[index] = intHostID
	}
	// auth: check authorization
	if err := s.AuthManager.AuthorizeByHostsIDs(ctx.Kit.Ctx, ctx.Kit.Header, authmeta.Update, hostIDArr...); err != nil {
		blog.Errorf("check host authorization failed, hosts: %+v, err: %v, rid: %s", hostIDArr, err, ctx.Kit.Rid)
		if err != nil && err != ac.NoAuthorizeError {
			blog.ErrorJSON("check host authorization failed, hosts: %s, err: %s, rid: %s", hostIDArr, err.Error(), ctx.Kit.Rid)
			ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommAuthorizeFailed))
			return
		}
		perm, err := s.AuthManager.GenHostBatchNoPermissionResp(ctx.Kit.Ctx, ctx.Kit.Header, authmeta.Update, hostIDArr)
		if err != nil && err != ac.NoAuthorizeError {
			blog.ErrorJSON("check host authorization get permission failed, hosts: %s, err: %s, rid: %s", hostIDArr, err.Error(), ctx.Kit.Rid)
			ctx.RespAutoError(ctx.Kit.CCError.CCError(common.CCErrCommAuthorizeFailed))
			return
		}
		ctx.RespEntityWithError(perm, ac.NoAuthorizeError)
		return
	}

	logPreContents := make(map[int64]*logics.HostLog, 0)
	for _, hostID := range hostIDArr {
		audit := lgc.NewHostLog(ctx.Kit.Ctx, ctx.Kit.SupplierAccount)
		logPreContents[hostID] = audit
	}

	txnErr := s.Engine.CoreAPI.CoreService().Txn().AutoRunTxn(ctx.Kit.Ctx, s.EnableTxn, ctx.Kit.Header, func() error {
		hasHostUpdateWithoutHostApplyFiled := false
		// 功能开关：更新主机属性时是否剔除自动应用字段
		ccLang := s.Language.CreateDefaultCCLanguageIf(util.GetLanguage(ctx.Kit.Header))
		if meta.HostUpdateWithoutHostApplyFiled == true {
			hostProperties, hasRules, err := s.getHostApplyRelatedFields(ctx, hostIDArr)
			if err != nil {
				blog.Errorf("UpdateImportHosts failed, getHostApplyRelatedFields failed, hostIDArr: %+v, err: %v, rid:%s", hostIDArr, err, ctx.Kit.Rid)
				return err
			}
			// get host attributes
			if hasRules == true {
				hasHostUpdateWithoutHostApplyFiled = true
				for index, hostInfo := range hosts {
					delete(hostInfo, common.BKHostIDField)
					intHostID := indexHostIDMap[index]
					updateData := make(map[string]interface{})
					for key, value := range hostInfo {
						properties, ok := hostProperties[intHostID]
						if ok == true && util.InStrArr(properties, key) {
							continue
						}
						updateData[key] = value
					}
					opt := &meta.UpdateOption{
						Condition: mapstr.MapStr{common.BKHostIDField: intHostID},
						Data:      mapstr.NewFromMap(updateData),
					}
					result, err := s.CoreAPI.CoreService().Instance().UpdateInstance(ctx.Kit.Ctx, ctx.Kit.Header, common.BKInnerObjIDHost, opt)
					if err != nil {
						blog.Errorf("UpdateImportHosts UpdateObject http do error, err: %v,input:%+v,param:%+v,rid:%s", err, hostList.HostInfo, opt, ctx.Kit.Rid)
						errMsg = append(errMsg, ccLang.Languagef("import_host_update_fail", index, err.Error()))
						continue
					}
					if !result.Result {
						blog.ErrorJSON("UpdateImportHosts failed, UpdateObject failed, param:%s, response: %s, rid:%s", opt, result, ctx.Kit.Rid)
						errMsg = append(errMsg, ccLang.Languagef("import_host_update_fail", index, result.ErrMsg))
						continue
					}
					successMsg = append(successMsg, strconv.FormatInt(index, 10))
				}
			}
		}

		if hasHostUpdateWithoutHostApplyFiled == false {
			for index, hostInfo := range hosts {
				delete(hostInfo, common.BKHostIDField)
				intHostID := indexHostIDMap[index]
				opt := &meta.UpdateOption{
					Condition: mapstr.MapStr{common.BKHostIDField: intHostID},
					Data:      mapstr.NewFromMap(hostInfo),
				}
				result, err := s.CoreAPI.CoreService().Instance().UpdateInstance(ctx.Kit.Ctx, ctx.Kit.Header, common.BKInnerObjIDHost, opt)
				if err != nil {
					blog.ErrorJSON("UpdateImportHosts UpdateInstance http do error, err: %v,input:%+v,param:%+v,rid:%s", err, hostList.HostInfo, opt, ctx.Kit.Rid)
					errMsg = append(errMsg, ccLang.Languagef("import_host_update_fail", index, err.Error()))
					continue
				}
				if !result.Result {
					blog.ErrorJSON("UpdateImportHosts failed, UpdateInstance failed, param:%s, response: %s, rid:%s", opt, result, ctx.Kit.Rid)
					errMsg = append(errMsg, ccLang.Languagef("import_host_update_fail", index, result.ErrMsg))
					continue
				}
				successMsg = append(successMsg, strconv.FormatInt(index, 10))
			}
		}

		logLastContents := make([]meta.AuditLog, 0)
		for _, hostID := range hostIDArr {
			audit := logPreContents[hostID]
			if err := audit.WithCurrent(ctx.Kit.Ctx, hostID, hostFields); err != nil {
				blog.Errorf("UpdateImportHosts, but get host[%d] pre data for audit failed, err: %v, rid: %s", hostID, err, ctx.Kit.Rid)
				return ctx.Kit.CCError.CCError(common.CCErrHostDetailFail)
			}
			hostModuleConfig, err := lgc.GetConfigByCond(ctx.Kit.Ctx, meta.HostModuleRelationRequest{HostIDArr: []int64{hostID}, Fields: []string{common.BKAppIDField}})
			if err != nil {
				blog.Errorf("UpdateImportHosts GetConfigByCond failed, id[%v], err: %v,input:%+v,rid:%s", hostID, err, hostList.HostInfo, ctx.Kit.Rid)
				return err
			}
			var appID int64
			if len(hostModuleConfig) > 0 {
				appID = hostModuleConfig[0].AppID
			}
			auditLog, err := audit.AuditLog(ctx.Kit.Ctx, hostID, appID, meta.AuditUpdate)
			if err != nil {
				blog.Errorf("UpdateImportHosts create audit log failed, id[%v], err: %v,rid:%s", hostID, err, ctx.Kit.Rid)
				return err
			}
			logLastContents = append(logLastContents, auditLog)
		}

		auditResp, err := s.CoreAPI.CoreService().Audit().SaveAuditLog(ctx.Kit.Ctx, ctx.Kit.Header, logLastContents...)
		if err != nil {
			blog.Errorf("UpdateImportHosts, but add host[%v] audit failed, err: %v, rid:%s", hostIDArr, err, ctx.Kit.Rid)
			return err
		}
		if !auditResp.Result {
			blog.Errorf("UpdateImportHosts, but add host[%v] audit failed, err: %v, rid:%s", hostIDArr, auditResp.ErrMsg, ctx.Kit.Rid)
			return auditResp.CCError()
		}
		return nil
	})

	if txnErr != nil {
		ctx.RespAutoError(txnErr)
		return
	}
	retData := map[string]interface{}{
		"error":   errMsg,
		"success": successMsg,
	}
	ctx.RespEntity(retData)
}
