// Strings for the upstream key / client token / request log pages added in
// v0.4.0 (merged into i18n.js). New keys only: existing keys live in i18n.js.
// Prefixes: kd. key drawer (and shared drawer parts), td. token drawer, tl. token list,
// ll. log list, kt. key tests.
export const en = {
  'kl.intRequired': 'Enter a whole number.',
  // shared drawer parts + upstream key drawer
  'kd.range.24h': 'Last 24 hours', 'kd.range.7d': 'Last 7 days', 'kd.range.30d': 'Last 30 days',
  'kd.requests': 'Requests', 'kd.errors': 'Errors', 'kd.cost': 'Cost', 'kd.tokens': 'Tokens',
  'kd.usage': 'Usage', 'kd.lastCheck': 'Last check', 'kd.config': 'Configuration', 'kd.recentFailures': 'Recent failures',
  'kd.noFailures': 'No recent failed requests', 'kd.openLog': 'Open this log', 'kd.allLogs': 'View all →',
  'kd.openHint': 'Click for details', 'kd.viewLogs': 'View logs', 'kd.added': 'added {when}',
  'kd.autoDisabledWhy': 'Auto-disabled: {reason}. Once fixed, click “Restore” so it is used again.',
  'kd.testDetails': 'Full result', 'kd.neverTestedDesc': 'This key has not been checked yet.', 'kd.testNow': 'Check now',
  'kd.prioWeight': 'Priority / weight', 'kd.concRpm': 'Max concurrency / requests per minute', 'kd.created': 'Added', 'kd.updated': 'Last changed',
  'kd.notFound': 'This upstream key has been deleted. Its history is below.',

  // key tests
  'kt.quickCheck': 'Quick check', 'kt.fullCheck': 'Full check',
  'kt.quickDesc': 'Only queries key info, no cost', 'kt.fullDesc': 'Also sends one minimal request, uses credit (≈ $0.0001)',
  'kt.zapTip': 'Quick check — only queries key info, no cost',
  'kt.testAll': 'Quick-check all', 'kt.testAllHint': 'Quick-check every upstream key (no cost)',
  'kt.testAllTitle': 'Quick-check every upstream key?',
  'kt.testAllMsg': 'Each key’s info is queried from xAI; no request is sent, so this costs nothing. Keys reported as invalid or blocked are automatically disabled.',
  'kt.bulkTitle': 'Check {n} selected key(s)?',
  'kt.fullCheckOpt': 'Full check', 'kt.fullCheckOptHint': ' — also sends one minimal request per key (uses credit, ≈ $0.0001 each)',
  'kt.noFullCheck': 'No full check',
  'kt.untested': 'Not checked', 'kt.autoTest': 'Quick check after saving (no cost)', 'tl.noQuota': 'No quota',

  // client token list
  'tl.guide': 'Integration guide', 'tl.statusFilter': 'Filter by status',
  'tl.status.all': 'All', 'tl.status.enabled': 'Enabled', 'tl.status.disabled': 'Disabled', 'tl.status.expired': 'Expired', 'tl.status.exhausted': 'Quota used up',
  'tl.bulkDeleteTitle': 'Delete {n} client token(s)?', 'tl.bulkDeleteMsg': 'These tokens stop working immediately. This cannot be undone. Logs and cost records are kept.',
  'tl.andMore': '… and {m} more',

  // client token drawer
  'td.openHint': 'Click for details', 'td.regenerate': 'Regenerate token value', 'td.regenTitle': 'Regenerate token value?',
  'td.regenMsg': 'The current token value of “{name}” stops working immediately.', 'td.newSecret': 'New token value for {name}',
  'td.expiresAt': 'expires {when}', 'td.expiredAt': 'expired {when}',
  'td.quota': 'Quota', 'td.used': 'Used', 'td.noQuota': 'No quota limit',
  'td.tabOverview': 'Overview', 'td.tabExamples': 'Integration examples',
  'td.secretNote': 'The token value is only shown when the token is created. If it is lost, regenerate it.',
  'td.notFound': 'This client token has been deleted. Its history is below.',

  // v0.4.0 schema v8: attempts, soft deletes, quota used vs total spend
  'kt.attemptsCol': 'Attempts / failed', 'kt.attempts': 'Attempts', 'kt.attemptErrors': 'Failed',
  'kt.attemptsHint': 'Upstream attempts made with this key — one client request can try several keys.',
  'kt.attShort': 'att.', 'kt.failShort': 'failed',
  'kd.deleteMsg': '“{name}” stops being used immediately. This cannot be undone. Logs and cost records are kept.',
  'kd.bulkDeleteMsg': 'The selected upstream keys stop being used immediately. This cannot be undone. Logs and cost records are kept.',
  'kd.deleted': 'Deleted', 'kd.deletedMark': '(deleted)',
  'td.deleteMsg': '“{name}” stops working immediately. This cannot be undone. Logs and cost records are kept.',
  'tl.quotaCol': 'Used / quota',
  'td.quotaUsed': 'Used', 
  'td.spent': 'Total spend', 

  // request logs
  'll.noKeys': 'No upstream key is available, so every request fails (503).', 'll.noKeysLink': 'View upstream keys',
};

export const zh = {
  'kl.intRequired': '请输入整数。',
  'kd.range.24h': '最近 24 小时', 'kd.range.7d': '最近 7 天', 'kd.range.30d': '最近 30 天',
  'kd.requests': '请求数', 'kd.errors': '失败数', 'kd.cost': '费用', 'kd.tokens': 'Token 数',
  'kd.usage': '用量', 'kd.lastCheck': '最近检测', 'kd.config': '配置', 'kd.recentFailures': '最近失败的请求',
  'kd.noFailures': '最近没有失败请求', 'kd.openLog': '打开这条日志', 'kd.allLogs': '查看全部 →',
  'kd.openHint': '点击查看详情', 'kd.viewLogs': '查看日志', 'kd.added': '{when}添加',
  'kd.autoDisabledWhy': '已自动禁用：{reason}。处理好之后点“恢复”，让它重新参与分配。',
  'kd.testDetails': '完整结果', 'kd.neverTestedDesc': '这个上游密钥还没有检测过。', 'kd.testNow': '立即检测',
  'kd.prioWeight': '优先级 / 权重', 'kd.concRpm': '最大并发 / 每分钟请求上限', 'kd.created': '添加时间', 'kd.updated': '最近修改',
  'kd.notFound': '这个上游密钥已删除，下面是它的历史记录。',

  'kt.quickCheck': '快速检测', 'kt.fullCheck': '完整检测',
  'kt.quickDesc': '只查询密钥信息，不消耗额度', 'kt.fullDesc': '额外发一条最小请求，消耗额度（约 $0.0001）',
  'kt.zapTip': '快速检测：只查询密钥信息，不消耗额度',
  'kt.testAll': '全部快速检测', 'kt.testAllHint': '对所有上游密钥做快速检测（不消耗额度）',
  'kt.testAllTitle': '对所有上游密钥做快速检测？',
  'kt.testAllMsg': '逐个向 xAI 查询密钥信息，不发送请求，不消耗额度。无效或被封禁的上游密钥会被自动禁用。',
  'kt.bulkTitle': '检测选中的 {n} 个上游密钥？',
  'kt.fullCheckOpt': '完整检测', 'kt.fullCheckOptHint': ' —— 每个上游密钥额外发一条最小请求（消耗额度，每个约 $0.0001）',
  'kt.noFullCheck': '未做完整检测',
  'kt.untested': '未检测', 'kt.autoTest': '保存后立即快速检测（不消耗额度）', 'tl.noQuota': '不限额',

  'tl.guide': '接入指南', 'tl.statusFilter': '按状态筛选',
  'tl.status.all': '全部', 'tl.status.enabled': '已启用', 'tl.status.disabled': '已禁用', 'tl.status.expired': '已过期', 'tl.status.exhausted': '额度用尽',
  'tl.bulkDeleteTitle': '删除 {n} 个客户端令牌？', 'tl.bulkDeleteMsg': '这些令牌会立即失效，此操作无法撤销。日志和费用记录会保留。',
  'tl.andMore': '…等 {n} 个',

  'td.openHint': '点击查看详情', 'td.regenerate': '重新生成令牌值', 'td.regenTitle': '重新生成令牌值？',
  'td.regenMsg': '“{name}” 当前的令牌值将立即失效。', 'td.newSecret': '{name} 的新令牌值',
  'td.expiresAt': '{when} 过期', 'td.expiredAt': '已于 {when} 过期',
  'td.quota': '额度', 'td.used': '已用', 'td.noQuota': '不限额',
  'td.tabOverview': '概要', 'td.tabExamples': '接入示例',
  'td.secretNote': '令牌值只在创建时显示，丢失后请重新生成。',
  'td.notFound': '这个客户端令牌已删除，下面是它的历史记录。',

  'kt.attemptsCol': '上游尝试 / 失败', 'kt.attempts': '上游尝试', 'kt.attemptErrors': '失败',
  'kt.attemptsHint': '用这个上游密钥发起的上游尝试次数。一个请求可以有多次上游尝试。',
  'kt.attShort': '次尝试', 'kt.failShort': '失败',
  'kd.deleteMsg': '“{name}”会立即停止使用，此操作无法撤销。日志和费用记录会保留。',
  'kd.bulkDeleteMsg': '选中的上游密钥会立即停止使用，此操作无法撤销。日志和费用记录会保留。',
  'kd.deleted': '已删除', 'kd.deletedMark': '（已删除）',
  'td.deleteMsg': '“{name}”会立即失效，此操作无法撤销。日志和费用记录会保留。',
  'tl.quotaCol': '已用 / 额度',
  'td.quotaUsed': '已用', 
  'td.spent': '累计费用', 

  'll.noKeys': '没有可用的上游密钥，所有请求都会失败（503）。', 'll.noKeysLink': '查看上游密钥',
};
