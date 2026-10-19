// v0.4.1 strings owned by one frontend agent (merged into i18n.js; keys here
// override i18n.js and i18n-lists.js). Keep en and zh complete.
export const en = {
  'data.avLegacyHint': 'Compact the database once; afterwards space freed by deleting logs is reused automatically',
  // key drawer: recent events (pm-spec 9.6)
  'kd.recentEvents': 'Recent events',
  'kd.noEvents': 'No records yet',
  'kd.openEvent': 'Open this record on the logs page',
  'kd.allEvents': 'View all →',
  // overview: events needing attention
  'ov.attnTitle': 'Events needing attention',
  'ov.attnAll': 'View all →',
  'ov.attnNone': 'All clear',
  'ov.attnOpen': 'Open on the logs page',
  'ov.tokSub': 'Input {i}, cache read {c}, output {o}',
  // short list times (pm-spec 10.3)
  'misc.justNow': 'just now',
  'misc.minAgo': '{n} min ago',
  'misc.hrAgo': '{n} hr ago',
  // data & storage
  'data.retentionSub': 'Request logs older than the retention period are deleted automatically; stats and operation records are not affected.',
  'settings.purgeBg': '',
  'settings.h.disk_warn_percent': 'When the data disk is this full (50–99%), the console shows a warning.',
  'data.compactSub': 'Returns the space freed by deleting logs to the disk, in the background; requests are not affected.',
  'data.willDelete': 'request logs will be deleted',
  'data.willDeleteApprox': 'request logs will be deleted',
  'data.willDeleteNone': 'No request logs are older than this date',
  'settings.deleteBefore': 'Delete request logs older than',
  'settings.purgeTitle': 'Delete old request logs?',
};

export const zh = {
  'data.avLegacyHint': '建议先压缩一次数据库，之后清理日志空出的空间会自动复用',
  'kd.recentEvents': '最近事件',
  'kd.noEvents': '暂无记录',
  'kd.openEvent': '在日志页打开这条记录',
  'kd.allEvents': '查看全部 →',
  'ov.attnTitle': '最近需要关注的事件',
  'ov.attnAll': '查看全部 →',
  'ov.attnNone': '一切正常',
  'ov.attnOpen': '在日志页打开',
  'ov.tokSub': '输入 {i}，缓存读 {c}，输出 {o}',
  'misc.justNow': '刚刚',
  'misc.minAgo': '{n} 分钟前',
  'misc.hrAgo': '{n} 小时前',
  'data.retentionSub': '超过保留天数的请求日志会自动删除，统计和操作记录不受影响。',
  'settings.purgeBg': '',
  'settings.h.disk_warn_percent': '数据盘使用率达到此值（50–99%）时，控制台会显示提示。',
  'data.compactSub': '把清理后空出的空间归还给磁盘，在后台进行，不影响请求转发。',
  'data.willDelete': '条请求日志将被删除',
  'data.willDeleteApprox': '条请求日志将被删除',
  'data.willDeleteNone': '没有早于此时间的请求日志',
  'settings.deleteBefore': '删除早于此时间的请求日志',
  'settings.purgeTitle': '删除旧的请求日志？',
};
