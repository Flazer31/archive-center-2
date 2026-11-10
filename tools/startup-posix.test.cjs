// Isolated shell behavior only: no database, network or package installation.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const bash = process.env.AC2_TEST_BASH || (process.platform === 'win32'
  ? 'C:/Program Files/Git/bin/bash.exe' : '/bin/bash');
const root = fs.mkdtempSync(path.join(path.resolve(__dirname, '..'), '.startup-posix-test-'));
const script = fs.readFileSync(path.join(__dirname, '../bin/start.sh'));
const write = (name, value) => { fs.writeFileSync(name, value, {mode: 0o700}); fs.chmodSync(name, 0o700); };
let count = 0;
function fixture(name, options = {}) {
  const dir = path.join(root, name);
  fs.mkdirSync(path.join(dir, 'bin'), {recursive: true});
  fs.mkdirSync(path.join(dir, 'fake'));
  if (options.platform) write(path.join(dir, 'fake/uname'), `#!/bin/sh\nprintf '%s\\n' '${options.platform}'\n`);
  if (options.platform === 'Darwin') write(path.join(dir, 'fake/brew'), '#!/bin/sh\nexit 0\n');
  // MSYS mkdir -p cannot traverse profile parents in the Windows sandbox.
  // Relativize only fixture-local arguments; use the real mkdir otherwise.
  if (process.platform === 'win32') write(path.join(dir, 'fake/mkdir'), `#!/bin/sh
for argument do
  shift
  case "$argument" in "$PWD"/*) set -- "$@" "./\${argument#"$PWD"/}" ;; *) set -- "$@" "$argument" ;; esac
done
exec /usr/bin/mkdir "$@"
`);
  fs.writeFileSync(path.join(dir, 'bin/start.sh'), script);
  write(path.join(dir, 'bin/ac2'), '#!/bin/sh\nexit 0\n');
  if (!options.dependency) write(path.join(dir, 'fake/mariadbd'), '#!/bin/sh\nexit 0\n');
  write(path.join(dir, 'fake/pkg'), '#!/bin/sh\nprintf "package output\\n"\nprintf "package error\\n" >&2\n' + (options.pkg || 'exit 0\n'));
  if (!options.noUpdater) write(path.join(dir, 'bin/ac2-updater'), `#!/bin/sh
case "$1" in
prepare-runner) ${options.prepare || 'printf \'%s\\n\' "$PWD/bin/ac2-updater"'} ;;
run)
  printf '%s\\n' "$@" > runner-args
  printf '%s' "\${AC2_DNS_SERVERS-}" > runner-dns
  ${options.run || ''}
  printf '[준비 완료] fake backend\\n'
  printf '[준비 완료] fake backend\\n' >> "$AC2_START_LOG"
  printf 'CONSOLE_ONLY_TOKEN\\n'
  exit ${options.runExit || 0} ;;
esac
`);
  return dir;
}
function run(dir, args = [], extraEnv = {}, input = '', termux = true) {
  const result = spawnSync(bash, ['-c', 'PATH="$PWD/fake:$PATH"; export PATH; ' + (termux ? 'PREFIX=fake; export PREFIX; ' : 'unset PREFIX; ') + 'exec bash bin/start.sh "$@"', 'test', ...args], {
    cwd: dir, encoding: 'utf8', timeout: 28000, input,
    env: {...process.env, AC2_DNS_SERVERS: '', AC2_DATA_DIR: 'shared-data', AC2_START_LOG: '', HOME: 'isolated-home', XDG_DATA_HOME: '', ...extraEnv},
  });
  if (result.error) throw result.error;
  return {...result, output: result.stdout + result.stderr};
}
function logs(dir) {
  return fs.readdirSync(dir).filter(name => /^start-.*\.log$/.test(name));
}
function check(name, fn) { fn(); count++; console.log(`PASS ${name}`); }
try {
  for (const entry of [
    {name: 'Termux resolv.conf nameservers and IPv6', content: '# comment\nnameserver 192.0.2.53\nnameserver 2001:db8::53\r\nnameserver\nsearch example.invalid\n', expected: '192.0.2.53,2001:db8::53'},
    {name: 'Termux missing resolv.conf uses AC1 defaults', expected: '1.1.1.1:53,8.8.8.8:53'},
    {name: 'Termux empty nameservers use AC1 defaults', content: '# no nameserver\nsearch example.invalid\nnameserver\n', expected: '1.1.1.1:53,8.8.8.8:53'},
    {name: 'Termux resolv.conf read failure uses AC1 defaults', directory: true, expected: '1.1.1.1:53,8.8.8.8:53'},
    {name: 'Termux explicit DNS takes precedence', content: 'nameserver 192.0.2.53\n', override: '127.0.0.1:15353', expected: '127.0.0.1:15353'},
  ]) {
    check(entry.name, () => {
      const dir = fixture(`dns-${count}`);
      if (entry.content !== undefined) {
        fs.mkdirSync(path.join(dir, 'fake/etc'));
        fs.writeFileSync(path.join(dir, 'fake/etc/resolv.conf'), entry.content);
      }
      if (entry.directory) fs.mkdirSync(path.join(dir, 'fake/etc/resolv.conf'), {recursive: true});
      const result = run(dir, [], {AC2_DNS_SERVERS: entry.override || ''});
      assert.equal(result.status, 0, result.output);
      assert.equal(fs.readFileSync(path.join(dir, 'runner-dns'), 'utf8'), entry.expected);
      assert.ok(result.stdout.includes(`[준비] 외부 API DNS: ${entry.expected}`));
      const log = fs.readFileSync(path.join(dir, 'shared-data/logs', logs(path.join(dir, 'shared-data/logs'))[0]), 'utf8');
      assert.ok(log.includes(`[준비] 외부 API DNS: ${entry.expected}`));
    });
  }
  check('non-Termux keeps the unset DNS override unchanged', () => {
    const dir = fixture('dns-non-termux');
    const result = run(dir, [], {}, '', false);
    assert.equal(result.status, 0, result.output);
    assert.equal(fs.readFileSync(path.join(dir, 'runner-dns'), 'utf8'), '');
    assert.ok(!result.stdout.includes('외부 API DNS:'));
  });
  for (const entry of [
    {name: 'Linux XDG shared data default', platform: 'Linux', env: {XDG_DATA_HOME: 'isolated-xdg'}, data: 'isolated-xdg/archive-center2/data'},
    {name: 'Linux HOME shared data default', platform: 'Linux', env: {}, data: 'isolated-home/.local/share/archive-center2/data'},
    {name: 'macOS HOME shared data default', platform: 'Darwin', env: {}, data: 'isolated-home/Library/Application Support/ArchiveCenter2/data'},
  ]) {
    check(entry.name, () => {
      const dir = fixture(`platform-${entry.platform}-${count}`, {platform: entry.platform});
      const result = run(dir, [], {AC2_DATA_DIR: '', ...entry.env}, '', false);
      assert.equal(result.status, 0, result.output);
      const logDir = path.join(dir, entry.data, 'logs');
      assert.equal(logs(logDir).length, 1);
      assert.ok(!fs.existsSync(path.join(dir, 'data')));
      assert.ok(!fs.existsSync(path.join(dir, 'shared-data')));
    });
  }
  check('normal completion, one log copy, no raw backend token tee', () => {
    const dir = fixture('normal');
    const result = run(dir);
    assert.equal(result.status, 0, result.output);
    assert.match(result.stdout.split('\n')[0], /백엔드를 시작합니다/);
    assert.match(result.stdout, /정상 종료됨/);
    assert.ok(fs.existsSync(path.join(dir, 'shared-data/logs')), result.output);
    assert.ok(!fs.existsSync(path.join(dir, 'data')), result.output);
    const log = fs.readFileSync(path.join(dir, 'shared-data/logs', logs(path.join(dir, 'shared-data/logs'))[0]), 'utf8');
    assert.equal((log.match(/fake backend/g) || []).length, 1);
    assert.ok(!log.includes('CONSOLE_ONLY_TOKEN'));
  });
  check('missing updater retains real failure and guidance', () => {
    const dir = fixture('missing', {noUpdater: true});
    const result = run(dir);
    assert.notEqual(result.status, 0);
    assert.match(result.output, /chmod.*ac2-updater/);
    assert.match(result.stdout, /오류: 업데이트 도구 준비/);
    const log = fs.readFileSync(path.join(dir, 'shared-data/logs', logs(path.join(dir, 'shared-data/logs'))[0]), 'utf8');
    assert.match(log, /chmod.*ac2-updater/);
    assert.match(log, /종료 코드: 1/);
  });
  check('dependency stdout+stderr, heartbeat and actual exit status', () => {
    const dir = fixture('dependency', {dependency: true, pkg: 'sleep 16\nexit 37\n'});
    const result = run(dir);
    assert.equal(result.status, 37, result.output);
    assert.match(result.stdout, /Termux에 MariaDB 서버·클라이언트를 설치합니다/);
    assert.match(result.stdout, /아직 진행 중입니다: MariaDB/);
    const log = fs.readFileSync(path.join(dir, 'shared-data/logs', logs(path.join(dir, 'shared-data/logs'))[0]), 'utf8');
    assert.match(log, /package output/);
    assert.match(log, /package error/);
    assert.match(log, /종료 코드: 37/);
  });
  check('last explicit data path, spaces and retention namespace', () => {
    const dir = fixture('data');
    const logDir = path.join(dir, 'chosen data/logs');
    fs.mkdirSync(logDir, {recursive: true});
    for (let i = 0; i < 14; i++) fs.writeFileSync(path.join(logDir, `start-20000101-${String(i).padStart(6, '0')}.log`), 'old');
    fs.writeFileSync(path.join(logDir, 'keep.txt'), 'keep');
    fs.writeFileSync(path.join(logDir, 'other.log'), 'keep');
    const result = run(dir, ['-data', 'ignored', '--data=chosen data']);
    assert.equal(result.status, 0, result.output);
    assert.equal(logs(logDir).length, 10);
    assert.equal(fs.readFileSync(path.join(logDir, 'keep.txt'), 'utf8'), 'keep');
    assert.equal(fs.readFileSync(path.join(logDir, 'other.log'), 'utf8'), 'keep');
    assert.ok(!fs.existsSync(path.join(dir, 'ignored')));
    assert.match(fs.readFileSync(path.join(dir, 'runner-args'), 'utf8'), /--data=chosen data/);
  });
  check('updater preparation preserves protocol and failure status', () => {
    const dir = fixture('prepare', {prepare: 'printf "original prepare failure\\n" >&2; printf "original prepare failure\\n" >> "$AC2_START_LOG"; exit 23'});
    const result = run(dir);
    assert.equal(result.status, 23, result.output);
    assert.match(result.output, /original prepare failure/);
    assert.match(result.stdout, /오류: 업데이트 도구 준비/);
    assert.ok(!fs.existsSync(path.join(dir, 'runner-args')));
  });
  check('backend nonzero status is preserved', () => {
    const dir = fixture('backend', {runExit: 19});
    const result = run(dir);
    assert.equal(result.status, 19, result.output);
    assert.match(result.stdout, /오류: 백엔드 실행/);
    assert.match(result.stdout, /종료 코드: 19/);
  });
  check('slow prepare heartbeat does not corrupt runner path protocol', () => {
    const dir = fixture('slow-prepare', {prepare: 'sleep 16; printf \'%s\\n\' "$PWD/bin/ac2-updater"'});
    const result = run(dir);
    assert.equal(result.status, 0, result.output);
    assert.match(result.stdout, /아직 진행 중입니다: 업데이트 도구 준비/);
    assert.ok(fs.existsSync(path.join(dir, 'runner-args')));
  });
  check('dependency tools keep stdin', () => {
    const dir = fixture('stdin', {dependency: true, pkg: 'IFS= read -r answer\nprintf "answer:%s\\n" "$answer"\n'});
    const result = run(dir, [], {}, 'interactive-answer\n');
    assert.equal(result.status, 0, result.output);
    assert.match(result.stdout, /answer:interactive-answer/);
  });
  check('TERM interrupts a dependency and preserves signal exit status', () => {
    const dir = fixture('signal', {dependency: true, pkg: 'kill -TERM "$PPID"\nexit 0\n'});
    const result = run(dir);
    assert.equal(result.status, 143, result.output);
    assert.match(result.stdout, /종료 요청으로 중단됨: MariaDB/);
  });
  check('TERM reaches the running backend and waits for its cleanup', () => {
    const dir = fixture('backend-signal', {run: `trap 'printf "cleaned\\n" > cleaned; exit 0' TERM
kill -TERM "$PPID"
while :; do sleep 1; done`});
    const result = run(dir);
    assert.equal(result.status, 143, result.output);
    assert.equal(fs.readFileSync(path.join(dir, 'cleaned'), 'utf8'), 'cleaned\n');
    assert.match(result.stdout, /종료 요청으로 중단됨: 백엔드 실행/);
  });
  check('unwritable data location uses private temporary log fallback', () => {
    const dir = fixture('fallback');
    fs.writeFileSync(path.join(dir, 'data-file'), 'not a directory');
    const temp = path.join(dir, 'tmp');
    fs.mkdirSync(temp);
    // MSYS accepts slash-delimited Windows drive paths in TMPDIR as well.
    const result = run(dir, ['-data', 'data-file'], {TMPDIR: temp.replaceAll('\\', '/')});
    assert.equal(result.status, 0, result.output);
    assert.equal(logs(path.join(temp, 'ac2-start-logs')).length, 1);
    assert.match(result.stdout, /ac2-start-logs/);
  });
  console.log(`${count} isolated POSIX shell checks passed (${os.platform()}; ${bash}).`);
} finally {
  // All fixture children have completed through spawnSync before this cleanup.
  fs.rmSync(root, {recursive: true, force: true});
}
