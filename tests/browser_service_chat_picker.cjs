const assert = require('node:assert/strict');
const puppeteer = require(process.env.PUPPETEER_MODULE || 'puppeteer');
(async () => {
 const browser = await puppeteer.launch({headless:true,args:['--no-sandbox']});
 try {
  const page=await browser.newPage();const errors=[];page.on('pageerror',e=>errors.push(e.message));
  await page.setRequestInterception(true);
  page.on('request',r=>r.url().endsWith('/api/version')?r.respond({status:200,contentType:'application/json',body:'{"version":"test","update":{"available":false}}'}):r.continue());
  await page.setCookie({name:process.env.SUBSCRIPTION_COOKIE_NAME,value:process.env.SUBSCRIPTION_SESSION,url:process.env.SUBSCRIPTION_URL});
  await page.goto(process.env.SUBSCRIPTION_URL,{waitUntil:'networkidle0'});
  await page.click('#notificationServicesBtn');await page.waitForSelector('#notificationServicesList button');await page.click('#notificationServicesList button');
  await page.waitForSelector('#serviceSubscriptionForm');await page.select('#serviceSubscriptionBot',process.env.SUBSCRIPTION_ACCOUNT);
  await page.evaluate(async()=>{
   const response=await fetch('/api/gateway/v1/bot-accounts',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({name:'Historical alias',token:'940001:subscription-secret'})});
   if(response.status!==201)throw Error('Alias registration failed');
   await loadNotificationService(notificationServiceDetail.service.id);
  });
  const lists=await page.evaluate(async()=>({bots:await (await fetch('/api/bots')).json(),options:Array.from(document.querySelectorAll('#serviceSubscriptionBot option')).map(o=>({text:o.textContent,bot:serviceAccounts.find(a=>a.id===Number(o.value)).native_bot_id}))}));
  assert.equal(lists.options.length,lists.bots.length);
  assert.deepEqual(lists.options.map(a=>a.bot).sort(),lists.bots.map(b=>b.id).sort());
  for(const b of lists.bots)assert(lists.options.find(a=>a.bot===b.id).text.includes(b.name));
  assert.equal(await page.$('#serviceAccountForm'),null);
  await page.select('#serviceSubscriptionBot',process.env.SUBSCRIPTION_ACCOUNT);
  await page.waitForFunction(()=>Array.from(document.querySelectorAll('#serviceSubscriptionDestination option')).some(o=>o.textContent.includes('Discovered')),{timeout:4000});
  const before=await page.evaluate(async()=>await (await fetch('/api/gateway/v1/destinations')).json());assert.equal(before.length,1);
  const value=await page.$$eval('#serviceSubscriptionDestination option',options=>options.find(o=>o.textContent.includes('Discovered')).value);
  await page.select('#serviceSubscriptionDestination',value);
  await page.evaluate(()=>updateServiceDestinationOptions());
  assert.equal(await page.$eval('#serviceSubscriptionDestination',e=>e.value),value);
  await page.evaluate(()=>{const original=window.fetch;let drop=true;window.fetch=async(...args)=>{const response=await original(...args);if(drop&&String(args[0]).endsWith('/subscriptions')&&args[1]?.method==='POST'){drop=false;throw new TypeError('Simulated lost response');}return response;};});
  await page.$eval('#serviceSubscriptionForm',f=>f.requestSubmit());
  await page.waitForSelector('.service-subscription-cancel');
  const after=await page.evaluate(async()=>await (await fetch('/api/gateway/v1/destinations')).json());assert.equal(after.length,2);assert.equal(after.find(d=>d.chat_id===-100940002).name,'Discovered "group" <img src=x onerror=alert(1)>');
  await page.waitForFunction(()=>Array.from(document.querySelectorAll('#serviceSubscriptionDestination option')).some(o=>o.textContent.includes('Discovered')&&o.disabled));
  assert((await page.$eval('#notificationServiceDetail',e=>e.textContent)).includes('Discovered'));
  await page.click('.service-subscription-cancel');await page.waitForFunction(()=>!document.querySelector('.service-subscription-cancel'));
  await page.waitForFunction(()=>Array.from(document.querySelectorAll('#serviceSubscriptionDestination option')).some(o=>o.textContent.includes('Discovered')&&!o.disabled));
  assert.deepEqual(errors,[]);console.log('Both known chats visible; explicit subscribe validates/registers group; subscribed state and cancellation passed.');
 } finally {await browser.close();}
})().catch(e=>{console.error(e);process.exit(1);});
