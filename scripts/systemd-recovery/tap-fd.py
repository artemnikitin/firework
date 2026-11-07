import os, fcntl, struct, subprocess, json, errno
name='tap-pr66-fd'
request=struct.pack('16sH',name.encode(),0x1002)+bytes(22)
fd=os.open('/dev/net/tun',os.O_RDWR)
try:
 fcntl.ioctl(fd,0x400454ca,request)
 before=json.loads(subprocess.check_output(['ip','-json','link','show','dev',name]))[0]['ifindex']
 subprocess.run(['ip','link','delete','dev',name],check=True)
 subprocess.run(['ip','tuntap','add','dev',name,'mode','tap'],check=True)
 after=json.loads(subprocess.check_output(['ip','-json','link','show','dev',name]))[0]['ifindex']
 assert before!=after
 try:
  fcntl.ioctl(fd,0x800454d2,bytes(40))
 except OSError as exc:
  assert exc.errno in (errno.EBADFD,errno.ENODEV),exc
  print(f'Confirmed: old TAP fd detached ({exc}); recreated same-name device uses new ifindex {after}, previously {before}.')
 else:
  raise AssertionError('old descriptor still connected')
finally:
 os.close(fd)
 subprocess.run(['ip','link','delete','dev',name],check=False)
