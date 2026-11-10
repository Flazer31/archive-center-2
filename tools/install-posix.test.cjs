// Exercises the installer with real ZIP extraction and SHA-256, without network
// or package-manager access. The fixture launcher records the inherited stdin.
const assert = require('node:assert/strict');
const {spawnSync} = require('node:child_process');
const {createHash} = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');

const bash = process.env.AC2_TEST_BASH || (process.platform === 'win32'
  ? 'C:/Program Files/Git/bin/bash.exe' : '/bin/bash');
const root = fs.mkdtempSync(path.join(path.resolve(__dirname, '..'), '.install-test-'));
const version = '1.0.0', folder = 'ac2-' + version;
function command(program, args, options) {
  const result = spawnSync(program, args, {encoding: 'utf8', timeout: 15000, ...options});
  if (result.error) throw result.error;
  return result;
}

// how: 'file' = sh install.sh --version; 'latest' = no --version (the script
// asks the releases API); 'pipe' = the script itself on stdin, as "curl … | sh"
// opt.tools: 'no-unzip' / 'shasum' run with a PATH that holds only wrappers of the
// needed tools (unzip missing with a recording apt-get; shasum instead of sha256sum);
// opt.latest: 'compact' (one-line JSON) or 'bad' (no tag); opt.star: "hash *name" list
function fixture(platform, arch, prefix, outcome, os = 'Linux', how = 'file', opt = {}) {
  const dir = fs.mkdtempSync(path.join(root, 'case-'));
  const file = (name, content) => fs.writeFileSync(path.join(dir, name), content, {mode: 0o700});
  const asset = `ac2-${version}-${platform}.zip`;
  // piped, the launcher has no stdin of its own to read (it gets the terminal when there is one)
  const launch = how === 'pipe' ? '#!/bin/sh\nprintf started > launched\n'
    : '#!/bin/sh\nIFS= read -r input\nprintf "%s" "$input" > launched\n';
  fs.mkdirSync(path.join(dir, 'fake'));
  fs.mkdirSync(path.join(dir, 'home/archive-center2/old'), {recursive: true});
  file('home/archive-center2/old/existing', 'preserved');
  file('home/shareddata', 'preserved');
  file('fake/uname', `#!/bin/sh
case "$1" in -s) echo '${os}' ;; -m) echo '${arch}' ;; *) exit 1 ;; esac
`);
  file('latest.json', opt.latest === 'bad' ? '{"message":"API rate limit exceeded"}' : opt.latest === 'compact'
    ? `{"url":"https://api.github.com/x","tag_name":"v${version}","name":"release"}`
    : `{\n  "url": "https://api.github.com/x",\n  "tag_name": "v${version}",\n  "name": "release"\n}\n`);
  file('fake/curl', `#!/bin/sh
set -eu
[ "$1" = -fsSL ] && [ "$3" = -o ]
case "$2" in
  https://api.github.com/repos/test-owner/test-repo/releases/latest)
    cp latest.json "$4" ;;
  https://github.com/test-owner/test-repo/releases/download/v${version}/SHA256SUMS.txt)
    cp fixture-sums "$4" ;;
  https://github.com/test-owner/test-repo/releases/download/v${version}/${asset})
    cp fixture.zip "$4" ;;
  *) exit 98 ;;
esac
printf '%s\\n' "$2" >> downloads
`);
  const archive = command(process.env.AC2_TEST_PYTHON || (process.platform === 'win32' ? 'python' : 'python3'),
    ['-c', 'import sys,zipfile\nwith zipfile.ZipFile("fixture.zip", "w") as z: z.writestr("01_AC2_start.sh", sys.argv[1])',
      launch + (outcome === 'launch-error' ? 'exit 7\n' : '')],
    {cwd: dir});
  assert.equal(archive.status, 0, archive.stderr);
  const hash = createHash('sha256').update(fs.readFileSync(path.join(dir, 'fixture.zip'))).digest('hex');
  file('fixture-sums', `${outcome === 'checksum' ? '0'.repeat(64) : hash} ${opt.star ? '*' : ' '}${asset}\r\n`);
  if (outcome === 'exists') {
    fs.mkdirSync(path.join(dir, 'home/archive-center2', folder));
    file(`home/archive-center2/${folder}/01_AC2_start.sh`, 'existing version');
  }
  fs.copyFileSync(path.join(__dirname, '../install.sh'), path.join(dir, 'install.sh'));
  const run = {
    file: 'exec sh ./install.sh --repo test-owner/test-repo --version "$1"',
    latest: 'exec sh ./install.sh --repo test-owner/test-repo',
    pipe: 'exec sh -s -- --repo test-owner/test-repo < install.sh',
  }[how];
  let path0 = 'PATH="$PWD/fake:$PATH"; export PATH; command -v unzip && command -v sha256sum || exit 97; ';
  if (opt.tools) {
    // only these tools exist: each wrapper runs the real one
    const need = ['sh', 'grep', 'sed', 'awk', 'mktemp', 'mkdir', 'rm', 'id', 'cp', 'cat', 'uname0']
      .concat(opt.tools === 'shasum' ? ['unzip'] : ['sha256sum']);
    fs.mkdirSync(path.join(dir, 'bin'));
    for (const n of need) {
      if (n === 'uname0') continue;
      const real = command(bash, ['-c', 'command -v ' + n]).stdout.trim();
      assert.ok(real, 'tool ' + n);
      file('bin/' + n, `#!/bin/sh\nexec '${real}' "$@"\n`);
    }
    const sha = command(bash, ['-c', 'command -v sha256sum']).stdout.trim();
    if (opt.tools === 'shasum') file('bin/shasum', `#!/bin/sh\n[ "$1" = -a ] && [ "$2" = 256 ] || exit 96\nshift 2\nexec '${sha}' "$@"\n`);
    file('bin/sudo', '#!/bin/sh\nexec "$@"\n');
    file('bin/apt-get', '#!/bin/sh\nprintf "%s\\n" "$*" >> apt-calls\n');
    path0 = 'PATH="$PWD/fake:$PWD/bin"; export PATH; ';
  }
  const result = command(bash,
    ['-c', path0 + run, 'test', version],
    {cwd: dir, input: 'terminal input\n', env: {...process.env, HOME: 'home', PREFIX: prefix}});
  const installed = path.join(dir, 'home/archive-center2', folder);
  const read = (name) => fs.readFileSync(path.join(dir, name), 'utf8');
  if (outcome === 'success' || outcome === 'launch-error') {
    assert.equal(result.status, outcome === 'success' ? 0 : 7, result.stdout + result.stderr);
    assert.equal(read('launched'), how === 'pipe' ? 'started' : 'terminal input');
    // the checksum list and the package; without --version the release lookup first
    assert.equal(read('downloads').trim().split('\n').length, how === 'file' ? 2 : 3);
    assert.equal(fs.existsSync(path.join(installed, '01_AC2_start.sh')), true);
  } else {
    assert.equal(result.status, 1, result.stdout + result.stderr);
    assert.equal(fs.existsSync(path.join(dir, 'launched')), false);
    assert.equal(fs.readdirSync(path.join(dir, 'home')).some((n) => n.startsWith('.ac2-install')), false);
    if (outcome === 'no-unzip') {
      // the tool was asked for, then the package could not be unpacked: no install folder is left
      assert.match(read('apt-calls'), /install -y unzip/);
      assert.match(result.stderr, /패키지를 풀지 못했습니다/);
      assert.equal(fs.existsSync(installed), false);
    } else if (outcome === 'bad-latest') {
      assert.match(result.stderr, /최신 버전/);
      assert.equal(fs.existsSync(installed), false);
    } else if (outcome === 'checksum') {
      assert.match(result.stderr, /SHA-256/);
      assert.equal(fs.existsSync(installed), false);
    } else if (outcome === 'exists') {
      // found before anything else: nothing downloaded, no tool installed, nothing added to the folder
      // nothing of the release is fetched (without --version: the version lookup alone)
      if (how === 'latest') assert.deepEqual(read('downloads').trim().split('\n'), ['https://api.github.com/repos/test-owner/test-repo/releases/latest']);
      else assert.equal(fs.existsSync(path.join(dir, 'downloads')), false);
      assert.equal(fs.existsSync(path.join(dir, 'apt-calls')), false);
      assert.equal(fs.readFileSync(path.join(installed, '01_AC2_start.sh'), 'utf8'), 'existing version');
      assert.deepEqual(fs.readdirSync(installed), ['01_AC2_start.sh']);
    } else {
      assert.match(result.stderr, new RegExp(os + '/' + arch));
      assert.equal(fs.existsSync(path.join(dir, 'downloads')), false);
      assert.equal(fs.existsSync(installed), false);
    }
  }
  // nothing else under HOME is touched and the work folder is gone
  assert.equal(read('home/shareddata'), 'preserved');
  assert.equal(read('home/archive-center2/old/existing'), 'preserved');
  assert.deepEqual(fs.readdirSync(path.join(dir, 'home')).sort(), ['archive-center2', 'shareddata']);
  console.log(`PASS ${platform} ${outcome} ${os} ${how}`);
}

try {
  fixture('android-arm64', 'aarch64', '/data/data/com.termux/files/usr', 'success');
  fixture('linux-amd64', 'x86_64', '', 'success');
  fixture('linux-arm64', 'aarch64', '', 'success');
  fixture('linux-amd64', 'x86_64', '', 'checksum');
  fixture('linux-amd64', 'x86_64', '', 'exists');
  fixture('linux-amd64', 'x86_64', '', 'launch-error');
  fixture('linux-amd64', 'i686', '', 'unsupported');
  fixture('darwin-arm64', 'arm64', '', 'success', 'Darwin');
  fixture('darwin-amd64', 'x86_64', '', 'success', 'Darwin');
  fixture('darwin-amd64', 'ppc', '', 'unsupported', 'Darwin');
  fixture('linux-amd64', 'x86_64', '', 'success', 'Linux', 'latest');
  fixture('linux-arm64', 'aarch64', '', 'success', 'Linux', 'pipe');
  fixture('linux-amd64', 'x86_64', '', 'checksum', 'Linux', 'pipe');
  fixture('linux-amd64', 'x86_64', '', 'exists', 'Linux', 'file', {tools: 'no-unzip'});
  fixture('linux-amd64', 'x86_64', '', 'exists', 'Linux', 'latest');
  fixture('linux-amd64', 'x86_64', '', 'exists', 'Linux', 'latest', {tools: 'no-unzip'});
  fixture('linux-amd64', 'x86_64', '', 'no-unzip', 'Linux', 'file', {tools: 'no-unzip'});
  fixture('darwin-arm64', 'arm64', '', 'success', 'Darwin', 'file', {tools: 'shasum'});
  fixture('linux-amd64', 'x86_64', '', 'success', 'Linux', 'latest', {latest: 'compact', star: true});
  fixture('linux-amd64', 'x86_64', '', 'bad-latest', 'Linux', 'latest', {latest: 'bad'});
} finally {
  fs.rmSync(root, {recursive: true, force: true});
}
