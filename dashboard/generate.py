#!/usr/bin/env python3
"""Octo 运营数据看板 - 静态 HTML 生成器"""

import subprocess
import json
import os
from datetime import datetime

MYSQL_CMD = "docker exec octo-mysql-1 mysql -uroot -ptsdd123456 --default-character-set=utf8mb4 -N -e"
OUTPUT_DIR = "/var/www/html/dashboard"

# 排除 Bot、系统账号、测试用户
EXCLUDE_UIDS = "uid NOT IN (SELECT robot_id FROM robot) AND uid NOT IN ('u_10000', 'botfather', 'fileHelper') AND name NOT LIKE '%测试%' AND username NOT LIKE 'test%' AND username NOT LIKE 'demo%'"

# 排除 E2E 测试创建的群组（创建者为测试账号或已删除的测试用户）
EXCLUDE_GROUPS = "creator NOT IN ('aee7e220529141bfa5e0ac4a3a3dd40d', 'd776fe3a8fbc4a6e8d8627eb2d829042')"

def query(sql):
    # Write SQL to temp file to avoid shell escaping issues
    import tempfile
    with tempfile.NamedTemporaryFile(mode='w', suffix='.sql', delete=False) as f:
        f.write(sql)
        tmpfile = f.name
    result = subprocess.run(
        f'docker exec -i octo-mysql-1 mysql -uroot -ptsdd123456 --default-character-set=utf8mb4 -N im < {tmpfile}',
        shell=True, capture_output=True, text=True
    )
    os.unlink(tmpfile)
    if result.returncode != 0:
        return []
    rows = []
    for line in result.stdout.strip().split('\n'):
        if line:
            rows.append(line.split('\t'))
    return rows

def get_metrics():
    data = {}
    
    # 核心指标（排除 Bot/测试/系统）
    r = query(f"SELECT COUNT(*) FROM user WHERE {EXCLUDE_UIDS}")
    data['total_users'] = int(r[0][0]) if r else 0
    
    r = query(f"SELECT COUNT(*) FROM user WHERE created_at >= CURDATE() AND {EXCLUDE_UIDS}")
    data['today_users'] = int(r[0][0]) if r else 0
    
    r = query(f"SELECT COUNT(*) FROM user_online WHERE online=1 AND uid IN (SELECT uid FROM user WHERE {EXCLUDE_UIDS})")
    data['online_users'] = int(r[0][0]) if r else 0
    
    # 消息：排除 Bot 发的消息
    r = query(f"SELECT COUNT(*) FROM message WHERE from_uid IN (SELECT uid FROM user WHERE {EXCLUDE_UIDS})")
    data['total_messages'] = int(r[0][0]) if r else 0
    
    r = query(f"SELECT COUNT(*) FROM message WHERE created_at >= CURDATE() AND from_uid IN (SELECT uid FROM user WHERE {EXCLUDE_UIDS})")
    data['today_messages'] = int(r[0][0]) if r else 0
    
    r = query("SELECT COUNT(*) FROM `group` WHERE " + EXCLUDE_GROUPS)
    data['total_groups'] = int(r[0][0]) if r else 0
    
    r = query("SELECT COUNT(*) FROM robot")
    data['total_bots'] = int(r[0][0]) if r else 0
    
    r = query(f"SELECT COUNT(*) FROM friend WHERE uid IN (SELECT uid FROM user WHERE {EXCLUDE_UIDS})")
    data['total_friends'] = int(r[0][0]) if r else 0
    
    r = query(f"SELECT COUNT(*) FROM device WHERE uid IN (SELECT uid FROM user WHERE {EXCLUDE_UIDS})")
    data['total_devices'] = int(r[0][0]) if r else 0
    
    # DAU - 今日发过消息的去重真实用户
    r = query(f"SELECT COUNT(DISTINCT from_uid) FROM message WHERE created_at >= CURDATE() AND from_uid IN (SELECT uid FROM user WHERE {EXCLUDE_UIDS})")
    data['dau'] = int(r[0][0]) if r else 0
    
    # 14日消息趋势（真实用户）
    r = query(f"SELECT DATE(created_at), COUNT(*) FROM message WHERE created_at >= DATE_SUB(CURDATE(), INTERVAL 14 DAY) AND from_uid IN (SELECT uid FROM user WHERE {EXCLUDE_UIDS}) GROUP BY DATE(created_at) ORDER BY DATE(created_at)")
    data['msg_trend'] = [{'date': row[0], 'count': int(row[1])} for row in r] if r else []
    
    # 14日注册趋势（真实用户）
    r = query(f"SELECT DATE(created_at), COUNT(*) FROM user WHERE created_at >= DATE_SUB(CURDATE(), INTERVAL 14 DAY) AND {EXCLUDE_UIDS} GROUP BY DATE(created_at) ORDER BY DATE(created_at)")
    data['user_trend'] = [{'date': row[0], 'count': int(row[1])} for row in r] if r else []
    
    # DAU趋势（真实用户）
    r = query(f"SELECT DATE(created_at), COUNT(DISTINCT from_uid) FROM message WHERE created_at >= DATE_SUB(CURDATE(), INTERVAL 14 DAY) AND from_uid IN (SELECT uid FROM user WHERE {EXCLUDE_UIDS}) GROUP BY DATE(created_at) ORDER BY DATE(created_at)")
    data['dau_trend'] = [{'date': row[0], 'count': int(row[1])} for row in r] if r else []
    
    # 群组详情
    r = query("SELECT g.name, (SELECT COUNT(*) FROM group_member gm WHERE gm.group_no=g.group_no AND gm.is_deleted=0) as total, (SELECT COUNT(*) FROM group_member gm WHERE gm.group_no=g.group_no AND gm.is_deleted=0 AND gm.robot=0) as humans, (SELECT COUNT(*) FROM group_member gm WHERE gm.group_no=g.group_no AND gm.is_deleted=0 AND gm.robot=1) as bots, (SELECT COUNT(*) FROM message m WHERE m.channel_id=g.group_no AND m.channel_type=2) as total_msgs, (SELECT COUNT(*) FROM message m WHERE m.channel_id=g.group_no AND m.channel_type=2 AND m.created_at >= DATE_SUB(NOW(), INTERVAL 7 DAY)) as week_msgs, (SELECT COUNT(*) FROM message m WHERE m.channel_id=g.group_no AND m.channel_type=2 AND m.created_at >= CURDATE()) as today_msgs, DATE(g.created_at) as created FROM `group` g WHERE " + EXCLUDE_GROUPS + " ORDER BY week_msgs DESC")
    data['groups'] = []
    if r:
        for row in r:
            data['groups'].append({
                'name': row[0],
                'total': int(row[1]),
                'humans': int(row[2]),
                'bots': int(row[3]),
                'total_msgs': int(row[4]),
                'week_msgs': int(row[5]),
                'today_msgs': int(row[6]),
                'created': row[7]
            })
    
    # 每小时消息分布(今日)
    r = query("SELECT HOUR(created_at), COUNT(*) FROM message WHERE created_at >= CURDATE() GROUP BY HOUR(created_at) ORDER BY HOUR(created_at)")
    data['hourly_msgs'] = [{'hour': int(row[0]), 'count': int(row[1])} for row in r] if r else []
    
    # Bot列表及消息数 + 归属者邮箱（排除系统Bot和E2E测试Bot）
    r = query("SELECT r.username, r.robot_id, (SELECT COUNT(*) FROM message m WHERE m.from_uid=r.robot_id AND m.created_at >= DATE_SUB(NOW(), INTERVAL 7 DAY)) as week_msgs, IFNULL((SELECT u.email FROM user u WHERE u.uid=r.creator_uid), '') as owner_email, IFNULL((SELECT u.name FROM user u WHERE u.uid=r.creator_uid), '') as owner_name FROM robot r WHERE r.username != 'botfather' AND r.username != '' AND r.creator_uid != '' AND r.creator_uid IN (SELECT uid FROM user) ORDER BY week_msgs DESC LIMIT 20")
    data['top_bots'] = [{'name': row[0], 'id': row[1], 'week_msgs': int(row[2]), 'owner_email': row[3], 'owner_name': row[4]} for row in r] if r else []
    
    data['updated_at'] = datetime.now().strftime('%Y-%m-%d %H:%M:%S')
    return data

def generate_html(data):
    msg_labels = json.dumps([d['date'] for d in data['msg_trend']])
    msg_values = json.dumps([d['count'] for d in data['msg_trend']])
    user_labels = json.dumps([d['date'] for d in data['user_trend']])
    user_values = json.dumps([d['count'] for d in data['user_trend']])
    dau_labels = json.dumps([d['date'] for d in data['dau_trend']])
    dau_values = json.dumps([d['count'] for d in data['dau_trend']])
    
    # Hourly - fill 24 hours
    hourly = [0] * 24
    for h in data['hourly_msgs']:
        hourly[h['hour']] = h['count']
    hourly_json = json.dumps(hourly)
    
    # Group table rows
    group_rows = ""
    for g in data['groups']:
        activity = "🔴 活跃" if g['week_msgs'] > 50 else ("🟡 一般" if g['week_msgs'] > 0 else "⚪ 沉默")
        group_rows += f"""
        <tr>
            <td>{g['name']}</td>
            <td>{g['total']}</td>
            <td>{g['humans']}</td>
            <td>{g['bots']}</td>
            <td>{g['total_msgs']}</td>
            <td>{g['week_msgs']}</td>
            <td>{g['today_msgs']}</td>
            <td>{activity}</td>
            <td>{g['created']}</td>
        </tr>"""
    
    # Bot table rows
    bot_rows = ""
    for b in data['top_bots']:
        owner = b.get('owner_email') or b.get('owner_name') or '(已删除用户)'
        bot_rows += f"<tr><td>{b['name']}</td><td>{owner}</td><td>{b['week_msgs']}</td></tr>"
    if not bot_rows:
        bot_rows = "<tr><td colspan='3' style='text-align:center;color:#666;'>暂无 Bot</td></tr>"

    # Compute derived metrics
    avg_friends = round(data['total_friends'] * 2 / max(data['total_users'], 1), 1)
    total_group_members = sum(g['total'] for g in data['groups'])
    total_group_bots = sum(g['bots'] for g in data['groups'])
    total_group_humans = sum(g['humans'] for g in data['groups'])
    bot_ratio = round(total_group_bots / max(total_group_members, 1) * 100, 1)

    html = f"""<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Octo 运营看板</title>
<script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.0/dist/chart.umd.min.js"></script>
<style>
  * {{ margin: 0; padding: 0; box-sizing: border-box; }}
  body {{ font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #0f1923; color: #e0e6ed; padding: 20px; }}
  .header {{ text-align: center; margin-bottom: 30px; }}
  .header h1 {{ font-size: 28px; color: #4fc3f7; margin-bottom: 5px; }}
  .header .update {{ color: #78909c; font-size: 13px; }}
  .cards {{ display: grid; grid-template-columns: repeat(auto-fit, minmax(160px, 1fr)); gap: 15px; margin-bottom: 30px; }}
  .card {{ background: #1a2733; border-radius: 12px; padding: 20px; text-align: center; border: 1px solid #263542; }}
  .card .value {{ font-size: 32px; font-weight: 700; color: #4fc3f7; }}
  .card .label {{ font-size: 13px; color: #78909c; margin-top: 5px; }}
  .card .sub {{ font-size: 12px; color: #546e7a; margin-top: 3px; }}
  .charts {{ display: grid; grid-template-columns: repeat(auto-fit, minmax(450px, 1fr)); gap: 20px; margin-bottom: 30px; }}
  .chart-box {{ background: #1a2733; border-radius: 12px; padding: 20px; border: 1px solid #263542; }}
  .chart-box h3 {{ color: #b0bec5; font-size: 15px; margin-bottom: 15px; }}
  .section {{ background: #1a2733; border-radius: 12px; padding: 20px; border: 1px solid #263542; margin-bottom: 20px; }}
  .section h3 {{ color: #b0bec5; font-size: 15px; margin-bottom: 15px; }}
  table {{ width: 100%; border-collapse: collapse; font-size: 13px; }}
  th {{ background: #263542; color: #90a4ae; padding: 10px 8px; text-align: left; font-weight: 500; }}
  td {{ padding: 10px 8px; border-bottom: 1px solid #263542; }}
  tr:hover {{ background: #1e3040; }}
  code {{ background: #263542; padding: 2px 6px; border-radius: 4px; font-size: 12px; }}
  .footer {{ text-align: center; color: #546e7a; font-size: 12px; margin-top: 30px; }}
  @media (max-width: 768px) {{
    .charts {{ grid-template-columns: 1fr; }}
    .cards {{ grid-template-columns: repeat(3, 1fr); }}
  }}
</style>
</head>
<body>

<div class="header">
  <h1>📊 Octo 运营看板</h1>
  <div class="update">更新时间: {data['updated_at']} · 每 5 分钟自动刷新</div>
</div>

<div class="cards">
  <div class="card">
    <div class="value">{data['total_users']}</div>
    <div class="label">总用户数</div>
    <div class="sub">今日 +{data['today_users']}</div>
  </div>
  <div class="card">
    <div class="value">{data['online_users']}</div>
    <div class="label">当前在线</div>
    <div class="sub">在线率 {round(data['online_users']/max(data['total_users'],1)*100)}%</div>
  </div>
  <div class="card">
    <div class="value">{data['dau']}</div>
    <div class="label">今日活跃(DAU)</div>
    <div class="sub">活跃率 {round(data['dau']/max(data['total_users'],1)*100)}%</div>
  </div>
  <div class="card">
    <div class="value">{data['total_messages']:,}</div>
    <div class="label">总消息数</div>
    <div class="sub">今日 +{data['today_messages']}</div>
  </div>
  <div class="card">
    <div class="value">{data['total_groups']}</div>
    <div class="label">群组数</div>
    <div class="sub">成员 {total_group_members} 人次</div>
  </div>
  <div class="card">
    <div class="value">{data['total_bots']}</div>
    <div class="label">Bot 数量</div>
    <div class="sub">群内占比 {bot_ratio}%</div>
  </div>
  <div class="card">
    <div class="value">{data['total_friends']}</div>
    <div class="label">好友关系</div>
    <div class="sub">人均 {avg_friends} 个好友</div>
  </div>
  <div class="card">
    <div class="value">{data['total_devices']}</div>
    <div class="label">注册设备</div>
    <div class="sub">人均 {round(data['total_devices']/max(data['total_users'],1),1)} 台</div>
  </div>
</div>

<div class="charts">
  <div class="chart-box">
    <h3>📈 每日消息量趋势</h3>
    <canvas id="msgChart"></canvas>
  </div>
  <div class="chart-box">
    <h3>👥 每日新增用户 & DAU</h3>
    <canvas id="userChart"></canvas>
  </div>
  <div class="chart-box">
    <h3>🕐 今日消息时段分布</h3>
    <canvas id="hourlyChart"></canvas>
  </div>
  <div class="chart-box">
    <h3>🤖 Bot 列表（7日消息）</h3>
    <table>
      <thead><tr><th>Bot 名称</th><th>归属者</th><th>7日消息</th></tr></thead>
      <tbody>{bot_rows}</tbody>
    </table>
  </div>
</div>

<div class="section">
  <h3>💬 群组详情</h3>
  <table>
    <thead>
      <tr><th>群名</th><th>总人数</th><th>人类</th><th>Bot</th><th>总消息</th><th>7日消息</th><th>今日</th><th>活跃度</th><th>创建日期</th></tr>
    </thead>
    <tbody>{group_rows}</tbody>
  </table>
</div>

<div class="footer">
  Octo V1 · Powered by OpenClaw 🐾
</div>

<script>
const chartOpts = {{
  responsive: true,
  plugins: {{ legend: {{ labels: {{ color: '#90a4ae' }} }} }},
  scales: {{
    x: {{ ticks: {{ color: '#78909c', maxRotation: 45 }}, grid: {{ color: '#263542' }} }},
    y: {{ ticks: {{ color: '#78909c' }}, grid: {{ color: '#263542' }}, beginAtZero: true }}
  }}
}};

new Chart(document.getElementById('msgChart'), {{
  type: 'line',
  data: {{
    labels: {msg_labels},
    datasets: [{{
      label: '消息数',
      data: {msg_values},
      borderColor: '#4fc3f7',
      backgroundColor: 'rgba(79,195,247,0.1)',
      fill: true,
      tension: 0.3
    }}]
  }},
  options: chartOpts
}});

new Chart(document.getElementById('userChart'), {{
  type: 'bar',
  data: {{
    labels: {user_labels},
    datasets: [
      {{ label: '新增用户', data: {user_values}, backgroundColor: 'rgba(129,199,132,0.7)' }},
      {{ label: 'DAU', data: {dau_values}, backgroundColor: 'rgba(255,183,77,0.7)' }}
    ]
  }},
  options: chartOpts
}});

new Chart(document.getElementById('hourlyChart'), {{
  type: 'bar',
  data: {{
    labels: Array.from({{length:24}}, (_,i) => i+':00'),
    datasets: [{{ label: '消息数', data: {hourly_json}, backgroundColor: 'rgba(79,195,247,0.6)' }}]
  }},
  options: chartOpts
}});

// Auto refresh every 5 min
setTimeout(() => location.reload(), 300000);
</script>
</body>
</html>"""
    return html

if __name__ == '__main__':
    os.makedirs(OUTPUT_DIR, exist_ok=True)
    data = get_metrics()
    html = generate_html(data)
    with open(os.path.join(OUTPUT_DIR, 'index.html'), 'w', encoding='utf-8') as f:
        f.write(html)
    print(f"Dashboard generated at {data['updated_at']}")
