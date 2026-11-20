// Пакет web — статика WebUI (SPEC 12/13, CODING ч.2). Директива go:embed не
// может подниматься выше каталога пакета, поэтому она живёт здесь, рядом с
// index.html; internal/webui импортирует этот FS и раздаёт его через свой mux.
// mock.js включён в бинарь сознательно: офлайн-режим разработки (DEMO=true в
// client.js) остаётся доступным без пересборки дерева.
package web

import "embed"

// FS — встроенное дерево статики: index.html + src/** (FSD-слои).
//
//go:embed index.html src
var FS embed.FS
