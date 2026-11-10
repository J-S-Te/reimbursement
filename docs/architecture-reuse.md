# 身份基础代码来源

OIDC、授权上下文、加密持久会话、PKCE、backchannel logout、中央会话治理复用项目仓模式。
源提交以开发起始 project_management 当前 HEAD 为准；业务适配改为 reimbursement / exp_oidc，数据范围在报销侧重新映射，不能把项目 AllowAll 带入报销。
internal/sessiongovernance为既有共享协议核心，保持源字节一致；internal/identity为报销适配，使用行为测试而不宣称字节一致。
