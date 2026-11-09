// Package web 嵌入管理控制台（八模块 SPA，对接 /api/admin/v2）。
// 文件即部署单元：index.html 壳 + app.css 主题/布局 + i18n.js 双语文案 +
// app.js 模块渲染。cmd/ir-agent-server 把 Assets 挂到 /admin/。
package web

import "embed"

// Assets 是控制台全部静态文件（无构建步骤，源码即产物）。
//
//go:embed index.html app.css app.js i18n.js share.html
var Assets embed.FS
