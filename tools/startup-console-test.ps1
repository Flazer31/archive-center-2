# Isolated console driver. Never reads console text or credential files.
param([ValidateSet('start','ctrl','key','members','owner')][string]$Action, [string]$CommandLine, [string]$Directory, [int]$TargetId)
$ErrorActionPreference = 'Stop'
Add-Type -TypeDefinition @'
using System;
using System.Text;
using System.Runtime.InteropServices;
using System.ComponentModel;
public static class StartupConsoleTest {
 [StructLayout(LayoutKind.Sequential)] struct Basic { public IntPtr reserved,peb,r2,r3,pid,parent; }
 [DllImport("ntdll.dll")] static extern int NtQueryInformationProcess(IntPtr h,int kind,out Basic info,int size,out int returned);
 public static int Parent(int pid) { using(var p=System.Diagnostics.Process.GetProcessById(pid)) { Basic b;int n; if(NtQueryInformationProcess(p.Handle,0,out b,Marshal.SizeOf(typeof(Basic)),out n)!=0)return 0;return b.parent.ToInt32(); } }
 public static int Owner(int pid) {
  foreach(var p in System.Diagnostics.Process.GetProcessesByName("cmd")) { using(p) { try { if(Parent(p.Id)==pid)return p.Id; } catch { } } }
  using(var p=System.Diagnostics.Process.GetProcessById(pid)) { return p.Id; }
 }
 [StructLayout(LayoutKind.Sequential, CharSet=CharSet.Unicode)] struct SI { public int cb; public string reserved, desktop, title; public int x,y,xs,ys,xc,yc,attr,flags; public short show, reserved2; public IntPtr reservedPtr, stdin,stdout,stderr; }
 [StructLayout(LayoutKind.Sequential)] struct PI { public IntPtr process, thread; public int pid, tid; }
 [StructLayout(LayoutKind.Explicit, Size=20)] struct Input { [FieldOffset(0)] public short type; [FieldOffset(4)] public int down; [FieldOffset(8)] public short repeat; [FieldOffset(10)] public short key; [FieldOffset(12)] public short scan; [FieldOffset(14)] public char value; [FieldOffset(16)] public int state; }
 [DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)] static extern bool CreateProcess(string app,StringBuilder command,IntPtr pa,IntPtr ta,bool inherit,uint flags,IntPtr env,string dir,ref SI si,out PI pi);
 [DllImport("kernel32.dll")] static extern bool CloseHandle(IntPtr h);
 [DllImport("kernel32.dll",SetLastError=true)] static extern bool AttachConsole(uint pid);
 [DllImport("kernel32.dll")] static extern bool FreeConsole();
 [DllImport("kernel32.dll")] static extern bool SetConsoleCtrlHandler(IntPtr handler,bool add);
 [DllImport("kernel32.dll",SetLastError=true)] static extern bool GenerateConsoleCtrlEvent(uint ev,uint group);
 [DllImport("kernel32.dll")] static extern uint GetConsoleProcessList(uint[] ids,uint count);
 [DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)] static extern IntPtr CreateFile(string name,uint access,uint share,IntPtr sec,uint creation,uint flags,IntPtr template);
 [DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)] static extern bool WriteConsoleInput(IntPtr h,Input[] input,uint count,out uint written);
 public static int Start(string command,string dir) {
  SetConsoleCtrlHandler(IntPtr.Zero,false); // Do not inherit a tool runner's ignored Ctrl+C.
  SI si=new SI();si.cb=Marshal.SizeOf(si);si.flags=1;si.show=0;
  string root=Environment.GetEnvironmentVariable("SystemRoot");
  string testRoot=Environment.GetEnvironmentVariable("AC2_CONSOLE_TEST_SYSTEMROOT");
  PI pi;
  try {
   if(!String.IsNullOrEmpty(testRoot))Environment.SetEnvironmentVariable("SystemRoot",testRoot);
   if(!CreateProcess(null,new StringBuilder(command),IntPtr.Zero,IntPtr.Zero,false,16,IntPtr.Zero,dir,ref si,out pi))throw new Win32Exception();
  } finally { Environment.SetEnvironmentVariable("SystemRoot",root); }
  CloseHandle(pi.thread);CloseHandle(pi.process);return pi.pid;
 }
 public static void Send(int pid,bool ctrl) {
  FreeConsole();if(!AttachConsole((uint)pid))throw new Win32Exception();
  SetConsoleCtrlHandler(IntPtr.Zero,true);
  if(ctrl) { if(!GenerateConsoleCtrlEvent(0,0))throw new Win32Exception(); System.Threading.Thread.Sleep(500); }
  else {
   IntPtr h=CreateFile("CONIN$",0xC0000000,3,IntPtr.Zero,3,0,IntPtr.Zero);
   Input[] items=new Input[2];items[0].type=1;items[0].down=1;items[0].repeat=1;items[0].key=32;items[0].value=' ';items[1]=items[0];items[1].down=0;
   uint written;if(!WriteConsoleInput(h,items,2,out written))throw new Win32Exception();CloseHandle(h);
  }
  FreeConsole();
 }
 public static uint[] Members(int pid) {
  FreeConsole();if(!AttachConsole((uint)pid))throw new Win32Exception();
  uint[] ids=new uint[100];uint count=GetConsoleProcessList(ids,100);FreeConsole();Array.Resize(ref ids,(int)count);return ids;
 }
}
'@
if ($Action -eq 'start') {
    if (-not $CommandLine) { $CommandLine = $env:AC2_CONSOLE_TEST_COMMAND }
    if (-not $Directory) { $Directory = $env:AC2_CONSOLE_TEST_DIRECTORY }
    [StartupConsoleTest]::Start($CommandLine, $Directory)
}
elseif ($Action -eq 'ctrl') { [StartupConsoleTest]::Send($TargetId, $true) }
elseif ($Action -eq 'members') { [StartupConsoleTest]::Members($TargetId) | ConvertTo-Json | Set-Content -Encoding UTF8 -LiteralPath $env:AC2_CONSOLE_TEST_MEMBERS }
elseif ($Action -eq 'owner') { [StartupConsoleTest]::Owner($TargetId) }
else { [StartupConsoleTest]::Send($TargetId, $false) }
