import json, datetime, os, sys
out = sys.argv[1]
os.makedirs(out, exist_ok=True)
tz = datetime.timezone(datetime.timedelta(hours=7))
def ts(day, hh, mm):
    return int(datetime.datetime(2026,10,day,hh,mm,tzinfo=tz).timestamp()*1000)
D="dom"
ME="@archive:dom"; OWNER="@whatsapp_84900000001:dom"; BOT="@whatsappbot:dom"
ME_MOM="@whatsapp_84911111111:dom"; BAP="@whatsapp_84922222222:dom"; LINH="@whatsapp_84933333333:dom"
ALICE="@alice:dom"; NOTICE="@server:dom"
n=[0]
def eid():
    n[0]+=1; return "$e%03d"%n[0]
def member(u, name, t=ts(1,0,0), membership="join"):
    c={"membership":membership}
    if name: c["displayname"]=name
    return {"type":"m.room.member","state_key":u,"sender":u,"event_id":eid(),"origin_server_ts":t,"content":c}
def st(typ, content, sender=ME, t=ts(1,0,0), key=""):
    return {"type":typ,"state_key":key,"sender":sender,"event_id":eid(),"origin_server_ts":t,"content":content}
def msg(sender, t, body, **extra):
    c={"msgtype":"m.text","body":body}; c.update(extra)
    return {"type":"m.room.message","sender":sender,"event_id":eid(),"origin_server_ts":t,"content":c,"unsigned":{}}
def raw(typ, sender, t, content, unsigned=None):
    return {"type":typ,"sender":sender,"event_id":eid(),"origin_server_ts":t,"content":content,"unsigned":unsigned or {}}

rooms={}; messages={}; state={}; events={}

# 1. WhatsApp DM with Mẹ (no room name: named after the contact)
m1=msg(ME_MOM, ts(31,18,2), "Con ăn cơm chưa?")
m0=msg(ME_MOM, ts(31,8,15), "Sáng nay trời mưa to quá")
m2=msg(OWNER, ts(31,18,5), "> <%s> Con ăn cơm chưa?\n\nRồi ạ" % ME_MOM, **{"m.relates_to":{"m.in_reply_to":{"event_id":m1["event_id"]}}})
m3=raw("m.room.message", ME_MOM, ts(31,18,7), {"msgtype":"m.audio","body":"Voice message.ogg","info":{"duration":42000},"org.matrix.msc3245.voice":{}})
m4=raw("m.room.message", ME_MOM, ts(31,18,9), {"msgtype":"m.image","body":"Bữa tối nay","filename":"IMG_2031.jpg"})
m5=msg(OWNER, ts(31,18,10), "Ngon qua")
m5e=raw("m.room.message", OWNER, ts(31,18,11), {"msgtype":"m.text","body":" * Ngon quá!","m.new_content":{"msgtype":"m.text","body":"Ngon quá!"},"m.relates_to":{"rel_type":"m.replace","event_id":m5["event_id"]}})
# reply to an old message not in the batch, without fallback (fetched via /event)
old=msg(ME_MOM, ts(25,9,0), "Nhớ gọi cho bà nội nhé")
events[old["event_id"]]=dict(old, room_id="!dm-me:dom")
m6=msg(ME_MOM, ts(31,18,20), "Con gọi chưa?", **{"m.relates_to":{"m.in_reply_to":{"event_id":old["event_id"]}}})
old_yesterday=msg(ME_MOM, ts(30,21,0), "Ngủ ngon con")
rooms["!dm-me:dom"]={
  "summary":{"m.heroes":[BOT, ME_MOM, OWNER],"m.joined_member_count":4,"m.invited_member_count":0},
  "state":{"events":[st("m.room.create",{"room_version":"10"},sender=BOT,t=ts(1,0,0)), member(ME_MOM,"Mẹ"), member(OWNER,"Arthur"), member(BOT,"WhatsApp bridge bot")]},
  "timeline":{"events":[old_yesterday,m0,m1,m2,m3,m4,m5,m5e,m6],"limited":False,"prev_batch":"dm-prev"}}

# 2. WhatsApp group: limited timeline -> backfill via /messages
G="!tango:dom"
gl=[]
for i in range(8):
    gl.append(msg([BAP,LINH,OWNER][i%3], ts(31,9,0)+i*600000, "msg %d about the ros2 bridge" % (i+5)))
gl[-1]=msg(BAP, ts(31,17,40), "pushed the ros2 bridge fix")
redacted=raw("m.room.message", LINH, ts(31,17,45), {}, {"redacted_because":{"type":"m.room.redaction","sender":LINH,"content":{}}})
meet=msg(LINH, ts(31,17,50), "Meeting moved to Monday 10:00, same room.\n\n\n\nBring the demo laptop.")
gl += [redacted, meet]
older=[msg(LINH, ts(31,8,0)-i*1800000, "older msg %d" % i) for i in range(4)]  # within 24h window (some), newest first
very_old=[msg(BAP, ts(29,10,0), "the meeting on Friday is cancelled")]
rooms[G]={
  "summary":{"m.heroes":[BAP, LINH, OWNER, BOT, "@whatsapp_84944444444:dom"],"m.joined_member_count":8},
  "state":{"events":[st("m.room.name",{"name":"TANGO team"}), st("m.room.create",{"room_version":"10"},t=ts(2,0,0)), member(BAP,"Baptiste"), member(LINH,"Linh")]},
  "timeline":{"events":gl,"limited":True,"prev_batch":"tango-prev"}}
messages[G]={
  "tango-prev":{"start":"tango-prev","end":"tango-p2","chunk":older,"state":[member(LINH,"Linh")]},
  "tango-p2":{"start":"tango-p2","end":"tango-p3","chunk":very_old,"state":[member(BAP,"Baptiste")]},
  "tango-p3":{"start":"tango-p3","chunk":[],"state":[]},
  "":{"start":"t-end","end":"tango-prev","chunk":list(reversed(gl)),"state":[member(BAP,"Baptiste"), member(LINH,"Linh"), member(OWNER,"Arthur")]},
}
state[G]=[st("m.room.name",{"name":"TANGO team"}), st("m.room.topic",{"topic":"TANGO project – robotics & ROS2"}), st("m.room.create",{"room_version":"10"},sender=BOT,t=ts(2,0,0)),
  member(ME,None), member(OWNER,"Arthur"), member(BOT,"WhatsApp bridge bot"), member(BAP,"Baptiste"), member(LINH,"Linh"),
  member("@whatsapp_84944444444:dom","Quang"), member("@whatsapp_84955555555:dom",None), member("@whatsapp_84966666666:dom","Hà"),
  member("@whatsapp_84977777777:dom","Left person", membership="leave")]

# 3. Bridge management room: only the bot -> hidden
rooms["!bridge:dom"]={"summary":{"m.heroes":[BOT],"m.joined_member_count":2},
  "state":{"events":[member(BOT,"WhatsApp bridge bot")]},
  "timeline":{"events":[raw("m.room.message",BOT,ts(31,6,0),{"msgtype":"m.notice","body":"Successfully logged in"})],"limited":False}}

# 4. Plain Matrix room with assorted event kinds
E="!lab:dom"
bundled_target=msg(ALICE, ts(31,10,0), "draft agenda")
bundled_target["unsigned"]={"m.relations":{"m.replace":{"type":"m.room.message","sender":ALICE,"event_id":"$edit-x","origin_server_ts":ts(31,10,30),"content":{"msgtype":"m.text","body":" * final agenda","m.new_content":{"msgtype":"m.text","body":"final agenda: demo, budget"},"m.relates_to":{"rel_type":"m.replace","event_id":bundled_target["event_id"]}}}}}
rooms[E]={"summary":{"m.heroes":[ALICE,"@bob:dom"],"m.joined_member_count":3},
  "state":{"events":[st("m.room.name",{"name":"Lab café"}), member(ALICE,"Alice")]},
  "timeline":{"events":[
     bundled_target,
     raw("m.room.encrypted",ALICE,ts(31,10,5),{"algorithm":"m.megolm.v1.aes-sha2","ciphertext":"xxx"}),
     raw("m.room.message",ALICE,ts(31,10,6),{"msgtype":"m.notice","body":"Build #42 passed"}),
     raw("m.room.message",ALICE,ts(31,10,7),{"msgtype":"m.emote","body":"waves"}),
     raw("m.room.message","@bob:dom",ts(31,10,8),{"msgtype":"m.file","body":"report.pdf"}),
     raw("m.room.message","@bob:dom",ts(31,10,9),{"msgtype":"m.location","body":"Location","geo_uri":"geo:10.77,106.70"}),
     raw("m.sticker",ALICE,ts(31,10,10),{"body":"thumbs up","url":"mxc://dom/x"}),
     raw("m.room.message","@bob:dom",ts(31,10,11),{"msgtype":"m.video","body":"clip.mp4"}),
     msg("@bob:dom", ts(31,10,12), "x"*620),
  ],"limited":False}}

# 5. Excluded by name regex; 6. space
rooms["!notices:dom"]={"summary":{"m.heroes":[NOTICE],"m.joined_member_count":2},"state":{"events":[st("m.room.name",{"name":"Server Notices"})]},
  "timeline":{"events":[raw("m.room.message",NOTICE,ts(31,7,0),{"msgtype":"m.notice","body":"maintenance tonight"})]}}
rooms["!space:dom"]={"summary":{"m.joined_member_count":1},"state":{"events":[st("m.room.name",{"name":"My space"}), st("m.room.create",{"type":"m.space"})]},"timeline":{"events":[]}}
# 7. Quiet old DM (Matrix), outside 24h
rooms["!old:dom"]={"summary":{"m.heroes":["@carol:dom"],"m.joined_member_count":2},"state":{"events":[]},
  "timeline":{"events":[msg("@carol:dom", ts(20,12,0), "see you in Đà Lạt next month")],"limited":False}}

messages["!dm-me:dom"]={"":{"start":"d0","end":"dm-prev","chunk":list(reversed(rooms["!dm-me:dom"]["timeline"]["events"])),"state":[member(ME_MOM,"Mẹ"),member(OWNER,"Arthur")]},
  "dm-prev":{"start":"dm-prev","end":"dm-p2","chunk":[old],"state":[]},"dm-p2":{"start":"dm-p2","chunk":[]}}
messages["!old:dom"]={"":{"start":"o0","chunk":list(reversed(rooms["!old:dom"]["timeline"]["events"]))}}
messages[E]={"":{"start":"l0","chunk":list(reversed(rooms[E]["timeline"]["events"])),"state":[member(ALICE,"Alice")]}}

# 8. Bridged WhatsApp DM as mautrix creates it: named room, so Synapse sends
# no heroes, and lazy loading only includes the last sender (me). The full
# member list (me, the contact's puppet, the bridge bot) comes from
# /joined_members, served from state.
BAP2="@whatsapp_84988888888:dom"; W="!bap-wa:dom"
w_msgs=[msg(BAP2, ts(29,20,30), "On se voit demain ?"), msg(ME, ts(29,20,33), "Oui, 10h au labo")]
rooms[W]={"summary":{"m.joined_member_count":3,"m.invited_member_count":0},
  "state":{"events":[st("m.room.name",{"name":"Baptiste (WA)"},sender=BOT), member(ME,None)]},
  "timeline":{"events":w_msgs[-1:],"limited":True,"prev_batch":"bap-prev"}}
state[W]=[st("m.room.name",{"name":"Baptiste (WA)"},sender=BOT), st("m.room.topic",{"topic":"WhatsApp private chat"},sender=BOT),
  st("m.room.create",{"room_version":"10"},sender=BOT,t=ts(29,17,30)), member(ME,None), member(BAP2,"Baptiste (WA)"), member(BOT,"WhatsApp bridge bot")]
messages[W]={"":{"start":"w0","end":"bap-prev","chunk":list(reversed(w_msgs)),"state":[member(BAP2,"Baptiste (WA)")]},"bap-prev":{"start":"bap-prev","chunk":[]}}

sync={"next_batch":"s1","rooms":{"join":rooms}}
search={"search_categories":{"room_events":{"count":2,"results":[
  {"rank":1,"result":dict(meet, room_id=G),"context":{"events_before":[redacted],"events_after":[]}},
  {"rank":1,"result":dict(very_old[0], room_id=G),"context":{"events_before":[],"events_after":[]}},
]}}}
profiles={ALICE:{"displayname":"Alice"},"@bob:dom":{"displayname":"Bob"},"@carol:dom":{"displayname":"Carol"}}
for name,obj in [("sync",sync),("messages",messages),("state",state),("events",events),("search",search),("profiles",profiles)]:
    json.dump(obj, open(os.path.join(out,name+".json"),"w"), ensure_ascii=False, indent=1)
print("now", ts(31,19,0))
