package main
import ("os";"testing")
func TestAgentBatch(t *testing.T){
  if os.Getenv("NANOCLAW_AGENT")!="1" { t.Skip() }
  dir:=os.Getenv("OUTDIR")
  cfg:=&Config{
    DeepseekKey:os.Getenv("DEEPSEEK_API_KEY"), DeepseekURL:os.Getenv("DEEPSEEK_API_URL"),
    Model:os.Getenv("NANOCLAW_MODEL"), VisionModel:"stepfun/step-3.7-flash", BraveKey:os.Getenv("BRAVE_API_KEY"),
    DataDir:dir, FocusChannels:map[string]bool{}, MaxToolIters:14, HistoryTurns:24,
    DiveToolIters:16, DivePasses:2, Coders:map[string]bool{}, RepoUsers:map[string]bool{},
  }
  a:=NewAgent(cfg)
  psa:="https://moxiecardshop.com/cdn/shop/files/CharizardV050PSA10.png?v=1684468758&width=1445"
  umb:="https://images.pokemontcg.io/sv8pt5/161_hires.png"
  cases:=[]struct{name,ch,msg string; imgs []string}{
    {"psa10-picture","c1","what card is this and what's it worth in this grade?",[]string{psa}},
    {"cardphoto-chart","c2","chart this card's price history",[]string{umb}},
    {"sealed","c3","price of a Prismatic Evolutions Elite Trainer Box (sealed)",nil},
    {"riftbound","c4","current price of Ahri Nine-Tailed Fox Signature in Riftbound Origins",nil},
    {"onepiece","c5","price of Monkey D. Luffy OP01-001 leader in One Piece",nil},
  }
  for _,c:=range cases{
    t.Logf("\n######### %s: %q #########", c.name, c.msg)
    r:=a.Handle(c.ch,"u1","aregus",c.msg,c.imgs...)
    t.Logf("[%s REPLY] %s", c.name, r.Text)
    for i,p:=range r.Artifacts{
      b,_:=os.ReadFile(p); os.WriteFile(dir+"/"+c.name+"-"+string(rune('a'+i))+".png",b,0o644)
      t.Logf("[%s ARTIFACT saved %d bytes]", c.name, len(b))
    }
  }
}
